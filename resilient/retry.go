package resilient

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gopkg.aoctech.app/api-commons/observability"
	"gopkg.aoctech.app/api-commons/ttlcache"
)

// Header names the client reads and writes.
const (
	HeaderIdempotencyKey = "Idempotency-Key"
	HeaderRequestID      = "X-Request-Id"
	headerRetryAfter     = "Retry-After"
	headerAuthorization  = "Authorization"
)

// Defaults of NewHTTPClient. Each one is overridable by an Option.
const (
	DefaultTimeout             = 6 * time.Second
	DefaultMaxAttempts         = 3
	DefaultBackoffBase         = 100 * time.Millisecond
	DefaultBackoffMax          = 2 * time.Second
	DefaultMaxRetryAfter       = 2 * time.Second
	DefaultDialTimeout         = 5 * time.Second
	DefaultTLSHandshakeTimeout = 5 * time.Second
	DefaultNegativeCacheTTL    = 5 * time.Second

	dialKeepAlive           = 30 * time.Second
	maxNegativeCacheEntries = 1024
	maxNegativeBodyBytes    = 64 << 10
	maxDrainBytes           = 4 << 10
	maxBackoffShift         = 30 // beyond this the shift overflows; the cap has long won
)

type options struct {
	timeout       time.Duration
	attempts      int
	backoffBase   time.Duration
	backoffMax    time.Duration
	maxRetryAfter time.Duration
	dialTimeout   time.Duration
	tlsTimeout    time.Duration
	headerTimeout time.Duration
	breaker       Config
	noBreaker     bool
	base          http.RoundTripper
	keyFn         func(*http.Request) string
	requestIDFn   func(context.Context) string
	negStatuses   map[int]bool
	negTTL        time.Duration
	sleep         func(context.Context, time.Duration) error
	jitter        func(ceiling time.Duration) time.Duration
}

// Option tunes NewHTTPClient and NewRetryTransport.
type Option func(*options)

// WithTimeout sets the total timeout of one call, retries included
// (NewHTTPClient only; NewRetryTransport has no client timeout).
func WithTimeout(d time.Duration) Option { return func(o *options) { o.timeout = d } }

// WithMaxAttempts sets the tries of an idempotent call (1 disables retry).
func WithMaxAttempts(n int) Option { return func(o *options) { o.attempts = n } }

// WithBackoff sets the exponential backoff: the n-th wait is a random time in
// [0, min(max, base*2^n)] (full jitter).
func WithBackoff(base, max time.Duration) Option {
	return func(o *options) { o.backoffBase, o.backoffMax = base, max }
}

// WithMaxRetryAfter caps the Retry-After that a retry is willing to wait. A longer
// one is not waited for: the response is returned as it is, so the caller decides
// instead of the client quietly ignoring what the server asked.
func WithMaxRetryAfter(d time.Duration) Option { return func(o *options) { o.maxRetryAfter = d } }

// WithDialTimeouts sets the transport's dial, TLS handshake and response header
// timeouts. A zero dial or TLS value keeps the default; a zero header timeout
// means none (the total timeout still bounds the call).
func WithDialTimeouts(dial, tlsHandshake, header time.Duration) Option {
	return func(o *options) {
		if dial > 0 {
			o.dialTimeout = dial
		}
		if tlsHandshake > 0 {
			o.tlsTimeout = tlsHandshake
		}
		o.headerTimeout = header
	}
}

// WithBreaker sets the circuit breaker config (see Config).
func WithBreaker(cfg Config) Option {
	return func(o *options) {
		if cfg.Key == nil {
			cfg.Key = o.breaker.Key
		}
		o.breaker = cfg
	}
}

// WithoutBreaker turns the circuit breaker off.
func WithoutBreaker() Option { return func(o *options) { o.noBreaker = true } }

// WithPerEndpointCircuit makes one circuit per host and path instead of per host.
func WithPerEndpointCircuit() Option { return func(o *options) { o.breaker.Key = EndpointKey } }

// WithBaseTransport replaces the transport underneath (tests, custom TLS). The
// dial and TLS options then do not apply.
func WithBaseTransport(rt http.RoundTripper) Option { return func(o *options) { o.base = rt } }

// WithIdempotencyKeyFunc injects an Idempotency-Key into every non-safe request
// that has none, using fn(req); an empty answer injects nothing. fn must be
// deterministic for the same logical operation (see DeterministicKey): a random
// key per call would let the server run a replayed operation twice. Without this
// option the client never invents a key. A request carrying a key becomes
// retryable, so only use it on endpoints that honor the header.
func WithIdempotencyKeyFunc(fn func(*http.Request) string) Option {
	return func(o *options) { o.keyFn = fn }
}

// WithRequestID overrides how X-Request-Id is found; the default is
// observability.RequestID(ctx). A request that already has the header keeps it.
func WithRequestID(fn func(context.Context) string) Option {
	return func(o *options) { o.requestIDFn = fn }
}

// WithNegativeCache remembers, for ttl, a safe call's answer when its status is
// one of statuses (typically 404), so a burst of lookups for something that does
// not exist hits the dependency once. Keyed by method, URL and a hash of the
// Authorization header, so one caller's answer is never served to another. Only
// GET and HEAD, only bodies up to 64 KiB. ttl <= 0 uses DefaultNegativeCacheTTL.
// The staleness window is ttl: keep it to seconds, and do not cache a status
// whose meaning flips right after a write the same process just made.
func WithNegativeCache(ttl time.Duration, statuses ...int) Option {
	return func(o *options) {
		if ttl <= 0 {
			ttl = DefaultNegativeCacheTTL
		}
		o.negTTL, o.negStatuses = ttl, map[int]bool{}
		for _, s := range statuses {
			o.negStatuses[s] = true
		}
	}
}

func newOptions(opts []Option) options {
	o := options{
		timeout: DefaultTimeout, attempts: DefaultMaxAttempts,
		backoffBase: DefaultBackoffBase, backoffMax: DefaultBackoffMax,
		maxRetryAfter: DefaultMaxRetryAfter,
		dialTimeout:   DefaultDialTimeout, tlsTimeout: DefaultTLSHandshakeTimeout,
		sleep: sleepCtx, jitter: randomUpTo,
	}
	for _, f := range opts {
		f(&o)
	}
	o.attempts = max(o.attempts, 1)
	return o
}

// HostKey is the default circuit key: the URL host.
func HostKey(req *http.Request) string { return req.URL.Host }

// EndpointKey is a circuit key of host and path.
func EndpointKey(req *http.Request) string { return req.URL.Host + req.URL.Path }

// DeterministicKey builds an Idempotency-Key from the parts that identify one
// logical operation (for example "wallet-debit", userID, hand ID). The same parts
// always give the same key, so a retry or a restarted job replays instead of
// repeating. The result is 64 hex characters, within the 128-character limit.
func DeterministicKey(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(strconv.Itoa(len(p)) + ":" + p)) // length prefix: ("ab","c") != ("a","bc")
	}
	return hex.EncodeToString(h.Sum(nil))
}

// NewHTTPClient is the one client a service uses to call another: the tuned
// transport, a circuit breaker (per host unless WithPerEndpointCircuit), retry
// of idempotent calls, and header injection. Order, outermost first: retry (with
// the headers), negative cache, breaker, transport. The breaker sits under the
// retry, so every attempt counts and an open circuit ends the retries at once.
//
// What is retried: a safe method (GET, HEAD, OPTIONS, TRACE), or any request that
// carries an Idempotency-Key, on a transport error or a 5xx other than 501/505,
// up to WithMaxAttempts tries. Never a 4xx (including 408 and 429): the request,
// not the server's luck, is what is wrong, and Retry-After on a 429 is for the
// caller to honor. A request whose body cannot be replayed (no GetBody) is sent
// once.
//
// Fail policy: the client fails closed. When the circuit is open or the retries
// are spent the caller gets the error (ErrCircuitOpen, the transport error) or the
// last response, never a guess. Whether "no answer" means allow (fail open) or
// deny is the caller's decision and belongs next to the call, e.g. a read of
// non-critical data may fail open, a money or PII mutation must fail closed.
func NewHTTPClient(opts ...Option) *http.Client {
	o := newOptions(opts)
	return &http.Client{Timeout: o.timeout, Transport: build(o)}
}

// NewRetryTransport is NewHTTPClient's RoundTripper, for a caller that builds its
// own http.Client (its own Timeout, CheckRedirect or Jar).
func NewRetryTransport(opts ...Option) http.RoundTripper { return build(newOptions(opts)) }

func dialer(timeout time.Duration) func(context.Context, string, string) (net.Conn, error) {
	return (&net.Dialer{Timeout: timeout, KeepAlive: dialKeepAlive}).DialContext
}

func build(o options) http.RoundTripper {
	rt := o.base
	if rt == nil {
		t := NewTransport()
		t.DialContext = dialer(o.dialTimeout)
		t.TLSHandshakeTimeout = o.tlsTimeout
		t.ResponseHeaderTimeout = o.headerTimeout
		rt = t
	}
	if !o.noBreaker {
		rt = Wrap(rt, o.breaker)
	}
	if o.negStatuses != nil {
		rt = &negativeTransport{base: rt, statuses: o.negStatuses, cache: ttlcache.New[negEntry](o.negTTL, maxNegativeCacheEntries)}
	}
	return &retryTransport{base: rt, o: o}
}

// retryTransport injects the headers and retries idempotent calls.
type retryTransport struct {
	base http.RoundTripper
	o    options
}

// isSafeMethod: the methods that change nothing on the server (RFC 9110 9.2.1).
func isSafeMethod(m string) bool {
	switch m {
	case "", http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	}
	return false
}

// retryableStatus: a 5xx that says "try again later". 501 and 505 will not change.
func retryableStatus(code int) bool {
	return code >= 500 && code != http.StatusNotImplemented && code != http.StatusHTTPVersionNotSupported
}

// prepare injects Idempotency-Key and X-Request-Id on a clone, never on the
// caller's request (a RoundTripper must not modify it).
func (t *retryTransport) prepare(req *http.Request) *http.Request {
	var key, rid string
	if req.Header.Get(HeaderIdempotencyKey) == "" && t.o.keyFn != nil && !isSafeMethod(req.Method) {
		key = t.o.keyFn(req)
	}
	if req.Header.Get(HeaderRequestID) == "" {
		if t.o.requestIDFn != nil {
			rid = t.o.requestIDFn(req.Context())
		} else {
			rid = observability.RequestID(req.Context())
		}
	}
	if key == "" && rid == "" {
		return req
	}
	req = req.Clone(req.Context())
	if key != "" {
		req.Header.Set(HeaderIdempotencyKey, key)
	}
	if rid != "" {
		req.Header.Set(HeaderRequestID, rid)
	}
	return req
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = t.prepare(req)
	replayable := req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
	canRetry := t.o.attempts > 1 && replayable &&
		(isSafeMethod(req.Method) || req.Header.Get(HeaderIdempotencyKey) != "")
	for attempt := 0; ; attempt++ {
		cur := req
		if attempt > 0 && req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			cur = req.Clone(req.Context())
			cur.Body = body
		}
		resp, err := t.base.RoundTrip(cur)
		if !canRetry || attempt+1 >= t.o.attempts || req.Context().Err() != nil {
			return resp, err
		}
		if err != nil {
			if errors.Is(err, ErrCircuitOpen) || errors.Is(err, context.Canceled) {
				return resp, err
			}
		} else if !retryableStatus(resp.StatusCode) {
			return resp, err
		}
		wait, ok := t.wait(resp, attempt)
		if !ok {
			return resp, err
		}
		if resp != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes))
			_ = resp.Body.Close()
		}
		if serr := t.o.sleep(req.Context(), wait); serr != nil {
			return nil, serr
		}
	}
}

// wait is the delay before the next attempt: the server's Retry-After when it sent
// one (false if it is longer than the cap), else a full-jitter exponential backoff.
func (t *retryTransport) wait(resp *http.Response, attempt int) (time.Duration, bool) {
	if resp != nil {
		if d, ok := parseRetryAfter(resp.Header.Get(headerRetryAfter), time.Now()); ok {
			return d, d <= t.o.maxRetryAfter
		}
	}
	ceiling := t.o.backoffMax
	if attempt < maxBackoffShift {
		ceiling = min(ceiling, t.o.backoffBase<<attempt)
	}
	return t.o.jitter(ceiling), true
}

func parseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if s, err := strconv.Atoi(v); err == nil && s >= 0 {
		return time.Duration(s) * time.Second, true
	}
	if at, err := http.ParseTime(v); err == nil {
		return max(0, at.Sub(now)), true
	}
	return 0, false
}

func randomUpTo(ceiling time.Duration) time.Duration {
	if ceiling <= 0 {
		return 0
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(ceiling)+1))
	if err != nil {
		return ceiling
	}
	return time.Duration(n.Int64())
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-tm.C:
		return nil
	}
}

type negEntry struct {
	status int
	header http.Header
	body   []byte
}

// negativeTransport briefly caches "not found"-style answers of safe reads.
type negativeTransport struct {
	base     http.RoundTripper
	statuses map[int]bool
	cache    *ttlcache.Cache[negEntry]
}

func negKey(req *http.Request) string {
	auth := sha256.Sum256([]byte(req.Header.Get(headerAuthorization)))
	return req.Method + " " + req.URL.String() + " " + hex.EncodeToString(auth[:8])
}

type bodyCloser struct {
	io.Reader
	io.Closer
}

func (n *negativeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return n.base.RoundTrip(req)
	}
	key := negKey(req)
	if e, ok := n.cache.Get(key); ok {
		return &http.Response{
			Status: strconv.Itoa(e.status) + " " + http.StatusText(e.status), StatusCode: e.status,
			Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
			Header: e.header.Clone(), Body: io.NopCloser(bytes.NewReader(e.body)),
			ContentLength: int64(len(e.body)), Request: req,
		}, nil
	}
	resp, err := n.base.RoundTrip(req)
	if err != nil || !n.statuses[resp.StatusCode] {
		return resp, err
	}
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxNegativeBodyBytes+1))
	if rerr != nil {
		_ = resp.Body.Close()
		return nil, rerr
	}
	if len(body) > maxNegativeBodyBytes {
		// Too big to keep: give back what was read plus the rest, uncached.
		resp.Body = bodyCloser{io.MultiReader(bytes.NewReader(body), resp.Body), resp.Body}
		return resp, nil
	}
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	n.cache.Set(key, negEntry{status: resp.StatusCode, header: resp.Header.Clone(), body: body})
	return resp, nil
}
