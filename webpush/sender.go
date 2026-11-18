package webpush

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gopkg.aoctech.app/api-commons/resilient"
	"gopkg.aoctech.app/api-commons/webhooks"
)

// Urgency is the RFC 8030 section 5.3 delivery priority.
type Urgency string

const (
	UrgencyVeryLow Urgency = "very-low"
	UrgencyLow     Urgency = "low"
	UrgencyNormal  Urgency = "normal"
	UrgencyHigh    Urgency = "high"
)

// maxThrottleWait is the longest Retry-After a 429 is waited out in place; a longer
// one drops the message for that subscription (push is best effort).
const maxThrottleWait = 2 * time.Second

// ErrUnknownKey reports a kid the Sender holds no VAPID key for.
var ErrUnknownKey = errors.New("webpush: unknown VAPID key id")

// ErrInvalidMessage reports a Message the push service would refuse.
var ErrInvalidMessage = errors.New("webpush: invalid message")

// Message is one push message.
type Message struct {
	// Payload is the plaintext, at most MaxPlaintext bytes (the product picks a smaller ceiling).
	Payload []byte
	// TTL is how long the push service keeps the message for an offline device; required, whole seconds, at least one.
	TTL     time.Duration
	Urgency Urgency
	// Topic replaces a pending message with the same topic: at most 32 URL-safe base64 characters.
	Topic string
	// DedupeKey identifies the logical delivery (the event id). It becomes the
	// Idempotency-Key, which is what lets the resilient client retry a transient failure
	// once. Without it a failed send is not retried.
	DedupeKey string
}

// Outcome classifies a push service answer for the caller; the zero value is not a valid outcome.
type Outcome int

const (
	// Delivered: the push service accepted the message (200, 201, 202).
	Delivered Outcome = iota + 1
	// Gone: 404 or 410, the subscription no longer exists; delete it.
	Gone
	// PayloadTooLarge: 413, a bug on our side; do not retry.
	PayloadTooLarge
	// Unauthorized: 401 or 403, the VAPID key does not match the subscription; delete it.
	Unauthorized
	// Throttled: 429 and the wait was longer than the Sender is willing to spend.
	Throttled
	// Rejected: any other 4xx (and a redirect); log, do not retry.
	Rejected
	// Transient: 5xx, timeout, network error or open circuit.
	Transient
)

func (o Outcome) String() string {
	switch o {
	case Delivered:
		return "delivered"
	case Gone:
		return "gone"
	case PayloadTooLarge:
		return "payload_too_large"
	case Unauthorized:
		return "unauthorized"
	case Throttled:
		return "throttled"
	case Rejected:
		return "rejected"
	case Transient:
		return "transient"
	}
	return "unknown"
}

// Result is what Send learned from the push service.
type Result struct {
	// Status is the HTTP status, 0 when no answer arrived.
	Status  int
	Outcome Outcome
	// RetryAfter is the push service's Retry-After on a 429, when it sent one.
	RetryAfter time.Duration
}

// Sender delivers messages with one or more VAPID keys.
type Sender struct {
	client  *http.Client
	subject string
	keys    map[string]Key
	now     func() time.Time
	sleep   func(context.Context, time.Duration) error
}

// NewClient is the HTTP client for push services: the guarded transport of
// webhooks.NewGuardedClient (no non-public address, no redirect, no proxy) under
// the resilient client (10 s timeout, a per-host circuit breaker, one retry of a
// transient failure for requests that carry an Idempotency-Key).
func NewClient() *http.Client {
	guarded := webhooks.NewGuardedClient(DefaultTimeout)
	c := resilient.NewHTTPClient(
		resilient.WithBaseTransport(guarded.Transport),
		resilient.WithTimeout(DefaultTimeout),
		resilient.WithMaxAttempts(2),
	)
	c.CheckRedirect = guarded.CheckRedirect
	return c
}

// NewSender builds a Sender. subject is the VAPID contact (mailto: or https: URL);
// keys are every VAPID key still in use, looked up by KID.
func NewSender(client *http.Client, subject string, keys ...Key) *Sender {
	m := make(map[string]Key, len(keys))
	for _, k := range keys {
		m[k.KID] = k
	}
	return &Sender{client: client, subject: subject, keys: m, now: time.Now, sleep: sleepCtx}
}

// Send encrypts and delivers one message to one subscription, signed with the
// key named by kid. The error is non-nil only when no answer was obtained (a
// message the service would refuse, a network failure, an open circuit); a
// refusal by the push service comes back as a Result.
func (s *Sender) Send(ctx context.Context, sub Subscription, kid string, m Message) (Result, error) {
	key, ok := s.keys[kid]
	if !ok {
		return Result{}, fmt.Errorf("%w: %q", ErrUnknownKey, kid)
	}
	ttl, err := checkMessage(m)
	if err != nil {
		return Result{}, err
	}
	body, err := Encrypt(sub, m.Payload)
	if err != nil {
		return Result{}, err
	}
	auth, err := VAPID(key, sub.Endpoint, s.subject, s.now())
	if err != nil {
		return Result{}, err
	}
	res, err := s.post(ctx, sub, body, auth, ttl, m)
	if err == nil && res.Outcome == Throttled && res.RetryAfter > 0 && res.RetryAfter <= maxThrottleWait {
		if serr := s.sleep(ctx, res.RetryAfter); serr != nil {
			return res, serr
		}
		return s.post(ctx, sub, body, auth, ttl, m)
	}
	return res, err
}

func (s *Sender) post(ctx context.Context, sub Subscription, body []byte, auth string, ttl int, m Message) (Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("%w: endpoint", ErrInvalidSubscription)
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", strconv.Itoa(ttl))
	if m.Urgency != "" {
		req.Header.Set("Urgency", string(m.Urgency))
	}
	if m.Topic != "" {
		req.Header.Set("Topic", m.Topic)
	}
	if m.DedupeKey != "" {
		req.Header.Set(resilient.HeaderIdempotencyKey, resilient.DeterministicKey("webpush", m.DedupeKey, sub.Endpoint))
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return Result{Outcome: Transient}, sendError(err, sub.Endpoint)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	return classify(resp, s.now()), nil
}

// sendError wraps a transport failure without the endpoint. http.Client.Do returns a *url.Error
// carrying the full URL, whose path is a per-subscription token (a credential); only the inner
// error is kept and the destination is named by host. errors.Is/As still reach the inner cause
// (context deadline, resilient.ErrCircuitOpen, a dial refusal).
func sendError(err error, endpoint string) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return fmt.Errorf("webpush: send to %s: %w", hostOf(endpoint), err)
}

func classify(resp *http.Response, now time.Time) Result {
	r := Result{Status: resp.StatusCode}
	switch c := resp.StatusCode; {
	case c == http.StatusOK || c == http.StatusCreated || c == http.StatusAccepted:
		r.Outcome = Delivered
	case c == http.StatusNotFound || c == http.StatusGone:
		r.Outcome = Gone
	case c == http.StatusRequestEntityTooLarge:
		r.Outcome = PayloadTooLarge
	case c == http.StatusUnauthorized || c == http.StatusForbidden:
		r.Outcome = Unauthorized
	case c == http.StatusTooManyRequests:
		r.Outcome = Throttled
		r.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), now)
	case c >= 500:
		r.Outcome = Transient
	default:
		r.Outcome = Rejected
	}
	return r
}

// parseRetryAfter reads delta-seconds or an HTTP date (RFC 9110 section 10.2.3); 0 when absent or invalid.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

func checkMessage(m Message) (ttlSeconds int, err error) {
	if m.TTL < time.Second {
		return 0, fmt.Errorf("%w: TTL must be at least one second", ErrInvalidMessage)
	}
	if len(m.Payload) > MaxPlaintext {
		return 0, fmt.Errorf("%w: %d bytes, max %d", ErrPayloadTooLarge, len(m.Payload), MaxPlaintext)
	}
	switch m.Urgency {
	case "", UrgencyVeryLow, UrgencyLow, UrgencyNormal, UrgencyHigh:
	default:
		return 0, fmt.Errorf("%w: urgency %q", ErrInvalidMessage, m.Urgency)
	}
	if len(m.Topic) > maxTopic || !urlSafe(m.Topic) {
		return 0, fmt.Errorf("%w: topic must be at most %d URL-safe base64 characters", ErrInvalidMessage, maxTopic)
	}
	return int(m.TTL / time.Second), nil
}

func urlSafe(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// String, GoString and LogValue redact: the payload is application content, so only its size is shown.
func (m Message) String() string {
	return fmt.Sprintf("webpush.Message{payload=%d bytes, ttl=%s, urgency=%s}", len(m.Payload), m.TTL, m.Urgency)
}
func (m Message) GoString() string     { return m.String() }
func (m Message) LogValue() slog.Value { return slog.StringValue(m.String()) }
