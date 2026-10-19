// Package resilient is the HTTP client every CTech service should use to call
// another service: a Transport tuned for it, and a circuit breaker per host.
//
// The default net/http transport keeps two idle connections per host, so a service
// that calls ctech-account from a few goroutines at once re-dials TLS on most
// calls; and a dependency that is down makes every caller wait for the full client
// timeout, holding a goroutine and a connection each, until the pile of waiting
// requests is a second outage. The breaker is what stops that: after a run of
// failures it answers at once with ErrCircuitOpen for a short cooldown, then lets
// a single request through to see whether the host is back.
//
// The breaker is per process, on purpose. It decides in memory on every call, with
// no shared store on the hot path; a process that is calling a failing host sees
// the failures itself, quickly, because it is the one with the traffic.
package resilient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// ErrCircuitOpen is what a call to a host with an open circuit returns (wrapped in
// *OpenError). Use errors.Is.
var ErrCircuitOpen = errors.New("resilient: circuit open")

// OpenError says which host is cut off and for how much longer at most.
type OpenError struct {
	Host       string
	RetryAfter time.Duration
}

func (e *OpenError) Error() string {
	return fmt.Sprintf("resilient: circuit open for %s, retry in %s", e.Host, e.RetryAfter.Round(time.Second))
}

func (e *OpenError) Is(target error) bool { return target == ErrCircuitOpen }

// State is a breaker's state for one host.
type State int

const (
	Closed State = iota
	Open
	HalfOpen
)

func (s State) String() string {
	switch s {
	case Open:
		return "open"
	case HalfOpen:
		return "half-open"
	}
	return "closed"
}

// Config tunes the breaker. The zero value is usable.
type Config struct {
	// Failures is how many failures in a row open the circuit. Default 5.
	Failures int
	// Cooldown is how long a circuit stays open before one probe is allowed.
	// Default 10 s. A failed probe doubles it, up to MaxCooldown (default 60 s).
	Cooldown    time.Duration
	MaxCooldown time.Duration
	// IsFailure says whether a call counts against the host. The default counts a
	// transport error (a timeout, a refused connection) and a 502, 503 or 504; a 4xx
	// or a 500 is the answer of a host that is up, and the caller cancelling its own
	// request is neither.
	IsFailure func(resp *http.Response, err error) bool
	// OnChange is told when a host's circuit changes state. Default: a log line.
	OnChange func(host string, from, to State)
	// Key picks the circuit a request belongs to. Default: the URL host (HostKey).
	// Use EndpointKey for one circuit per host and path, so a failing endpoint does
	// not cut off its healthy siblings. OnChange and OpenError then carry that key
	// in their Host field.
	Key func(req *http.Request) string

	now func() time.Time
}

func (c Config) withDefaults() Config {
	if c.Failures <= 0 {
		c.Failures = 5
	}
	if c.Cooldown <= 0 {
		c.Cooldown = 10 * time.Second
	}
	if c.MaxCooldown <= 0 {
		c.MaxCooldown = 60 * time.Second
	}
	if c.IsFailure == nil {
		c.IsFailure = defaultIsFailure
	}
	if c.OnChange == nil {
		c.OnChange = func(host string, from, to State) {
			slog.Warn("resilient: circuit state changed", "host", host, "from", from.String(), "to", to.String())
		}
	}
	if c.Key == nil {
		c.Key = HostKey
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c
}

func defaultIsFailure(resp *http.Response, err error) bool {
	if err != nil {
		// The caller giving up is not the host failing.
		return !errors.Is(err, context.Canceled)
	}
	switch resp.StatusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// NewTransport is a transport for calling other services: enough idle connections
// per host that concurrent calls reuse them (the default keeps two), HTTP/2 where
// the host offers it, and bounded dial and handshake times.
func NewTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// Wrap puts a per-host circuit breaker in front of base.
func Wrap(base http.RoundTripper, cfg Config) http.RoundTripper {
	return &breakerTransport{base: base, cfg: cfg.withDefaults()}
}

// Client is an http.Client over NewTransport with the breaker, and a total timeout.
func Client(timeout time.Duration, cfg Config) *http.Client {
	return &http.Client{Timeout: timeout, Transport: Wrap(NewTransport(), cfg)}
}

type breakerTransport struct {
	base  http.RoundTripper
	cfg   Config
	hosts sync.Map // host -> *hostBreaker
}

func (t *breakerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := t.cfg.Key(req)
	v, ok := t.hosts.Load(host)
	if !ok {
		v, _ = t.hosts.LoadOrStore(host, &hostBreaker{host: host, cooldown: t.cfg.Cooldown})
	}
	b := v.(*hostBreaker)

	probe, retryAfter, allowed := b.admit(t.cfg)
	if !allowed {
		return nil, &OpenError{Host: host, RetryAfter: retryAfter}
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil && errors.Is(err, context.Canceled) {
		b.abandon(probe)
		return resp, err
	}
	b.record(t.cfg, t.cfg.IsFailure(resp, err), probe)
	return resp, err
}

// hostBreaker is one host's state. All of it is guarded by mu; nothing blocks while
// holding it.
type hostBreaker struct {
	host string

	mu        sync.Mutex
	state     State
	failures  int       // consecutive, while closed
	openUntil time.Time // while open
	probing   bool      // a probe is in flight (half-open)
	cooldown  time.Duration
}

// admit decides whether a call may go. probe is true for the one call let through
// to test an open circuit.
func (b *hostBreaker) admit(cfg Config) (probe bool, retryAfter time.Duration, allowed bool) {
	var change *[2]State
	b.mu.Lock()
	now := cfg.now()
	switch b.state {
	case Closed:
		allowed = true
	case Open:
		if now.Before(b.openUntil) {
			retryAfter = b.openUntil.Sub(now)
			break
		}
		b.state, b.probing = HalfOpen, true
		change = &[2]State{Open, HalfOpen}
		probe, allowed = true, true
	case HalfOpen:
		if b.probing {
			retryAfter = time.Second
			break
		}
		b.probing = true
		probe, allowed = true, true
	}
	b.mu.Unlock()
	if change != nil {
		cfg.OnChange(b.host, change[0], change[1])
	}
	return probe, retryAfter, allowed
}

// record folds one call's outcome into the state.
func (b *hostBreaker) record(cfg Config, failed, probe bool) {
	var change *[2]State
	b.mu.Lock()
	now := cfg.now()
	switch {
	case probe && failed:
		b.cooldown = min(b.cooldown*2, cfg.MaxCooldown)
		b.state, b.probing, b.openUntil = Open, false, now.Add(b.cooldown)
		change = &[2]State{HalfOpen, Open}
	case probe:
		b.state, b.probing, b.failures, b.cooldown = Closed, false, 0, cfg.Cooldown
		change = &[2]State{HalfOpen, Closed}
	case failed && b.state == Closed:
		b.failures++
		if b.failures >= cfg.Failures {
			b.state, b.openUntil = Open, now.Add(b.cooldown)
			change = &[2]State{Closed, Open}
		}
	case !failed && b.state == Closed:
		b.failures = 0
	}
	b.mu.Unlock()
	if change != nil {
		cfg.OnChange(b.host, change[0], change[1])
	}
}

// abandon releases a probe whose request the caller cancelled: it says nothing
// about the host, so the next call probes again.
func (b *hostBreaker) abandon(probe bool) {
	if !probe {
		return
	}
	b.mu.Lock()
	b.probing = false
	b.mu.Unlock()
}
