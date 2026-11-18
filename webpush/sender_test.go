package webpush

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.aoctech.app/api-commons/resilient"
)

// newTarget starts a TLS push service double and a Sender that trusts it. The guarded client is not used
// here (it refuses loopback by design; TestNewClientRefusesLoopback covers that).
func newTarget(t *testing.T, h http.HandlerFunc, clientOpts ...resilient.Option) (*Sender, Subscription, Key, *httptest.Server) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	_, key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	sub, _ := newTestSubscription(t)
	sub.Endpoint = srv.URL + "/push/token"
	client := srv.Client()
	if len(clientOpts) > 0 {
		client = resilient.NewHTTPClient(append([]resilient.Option{resilient.WithBaseTransport(srv.Client().Transport)}, clientOpts...)...)
	}
	s := NewSender(client, "mailto:ops@example.com", key)
	s.sleep = func(context.Context, time.Duration) error { return nil }
	return s, sub, key, srv
}

func msg() Message { return Message{Payload: []byte("hi"), TTL: 90 * time.Second} }

func TestSendResponseTable(t *testing.T) {
	cases := []struct {
		status int
		want   Outcome
	}{
		{200, Delivered}, {201, Delivered}, {202, Delivered},
		{404, Gone}, {410, Gone},
		{413, PayloadTooLarge},
		{401, Unauthorized}, {403, Unauthorized},
		{429, Throttled},
		{400, Rejected}, {405, Rejected}, {302, Rejected},
		{500, Transient}, {502, Transient}, {503, Transient},
	}
	for _, c := range cases {
		t.Run(strconv.Itoa(c.status), func(t *testing.T) {
			s, sub, key, _ := newTarget(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(c.status) })
			res, err := s.Send(context.Background(), sub, key.KID, msg())
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if res.Outcome != c.want || res.Status != c.status {
				t.Errorf("got %v/%d, want %v/%d", res.Outcome, res.Status, c.want, c.status)
			}
		})
	}
}

func TestSendRequestShape(t *testing.T) {
	var got *http.Request
	var bodyLen int
	s, sub, key, _ := newTarget(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		buf := make([]byte, 5000)
		n, _ := r.Body.Read(buf)
		bodyLen = n
		w.WriteHeader(201)
	})
	m := msg()
	m.Urgency, m.Topic = UrgencyHigh, "nfe-123"
	if _, err := s.Send(context.Background(), sub, key.KID, m); err != nil {
		t.Fatal(err)
	}
	h := got.Header
	if got.Method != http.MethodPost || got.URL.Path != "/push/token" {
		t.Errorf("request = %s %s", got.Method, got.URL.Path)
	}
	for k, want := range map[string]string{
		"Ttl": "90", "Content-Encoding": "aes128gcm", "Content-Type": "application/octet-stream",
		"Urgency": "high", "Topic": "nfe-123",
	} {
		if h.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, h.Get(k), want)
		}
	}
	if a := h.Get("Authorization"); !strings.HasPrefix(a, "vapid t=ey") {
		t.Errorf("Authorization = %q", a)
	}
	if bodyLen == 0 || bodyLen > RecordSize {
		t.Errorf("body length = %d", bodyLen)
	}
	if h.Get("Idempotency-Key") != "" {
		t.Error("Idempotency-Key set without a DedupeKey")
	}
}

func TestSendRejectsBeforeTheNetwork(t *testing.T) {
	var hits atomic.Int32
	s, sub, key, _ := newTarget(t, func(w http.ResponseWriter, r *http.Request) { hits.Add(1) })
	ctx := context.Background()
	bad := map[string]Message{
		"no ttl":       {Payload: []byte("x")},
		"sub-second":   {Payload: []byte("x"), TTL: 500 * time.Millisecond},
		"urgency":      {Payload: []byte("x"), TTL: time.Minute, Urgency: "asap"},
		"topic chars":  {Payload: []byte("x"), TTL: time.Minute, Topic: "a b"},
		"topic length": {Payload: []byte("x"), TTL: time.Minute, Topic: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}
	for name, m := range bad {
		if _, err := s.Send(ctx, sub, key.KID, m); !errors.Is(err, ErrInvalidMessage) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	big := Message{Payload: make([]byte, MaxPlaintext+1), TTL: time.Minute}
	if _, err := s.Send(ctx, sub, key.KID, big); !errors.Is(err, ErrPayloadTooLarge) {
		t.Errorf("oversized: err = %v", err)
	}
	if _, err := s.Send(ctx, sub, "nope", msg()); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("unknown kid: err = %v", err)
	}
	if hits.Load() != 0 {
		t.Errorf("%d requests reached the server", hits.Load())
	}
}

func TestSendThrottled(t *testing.T) {
	var hits atomic.Int32
	var waited time.Duration
	s, sub, key, _ := newTarget(t, func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			return
		}
		w.WriteHeader(201)
	})
	s.sleep = func(_ context.Context, d time.Duration) error { waited = d; return nil }
	res, err := s.Send(context.Background(), sub, key.KID, msg())
	if err != nil || res.Outcome != Delivered || hits.Load() != 2 || waited != time.Second {
		t.Errorf("short Retry-After: %+v err=%v hits=%d waited=%v", res, err, hits.Load(), waited)
	}

	// A wait above the cap drops the message and reports it.
	hits.Store(0)
	s2, sub2, key2, _ := newTarget(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(429)
	})
	res, err = s2.Send(context.Background(), sub2, key2.KID, msg())
	if err != nil || res.Outcome != Throttled || res.RetryAfter != 30*time.Second || hits.Load() != 1 {
		t.Errorf("long Retry-After: %+v err=%v hits=%d", res, err, hits.Load())
	}

	// A 429 that keeps coming is not retried more than once.
	hits.Store(0)
	s3, sub3, key3, _ := newTarget(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(429)
	})
	res, _ = s3.Send(context.Background(), sub3, key3.KID, msg())
	if res.Outcome != Throttled || hits.Load() != 2 {
		t.Errorf("repeated 429: %+v hits=%d", res, hits.Load())
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	cases := map[string]time.Duration{
		"": 0, "5": 5 * time.Second, "-1": 0, "abc": 0,
		now.Add(90 * time.Second).Format(http.TimeFormat): 90 * time.Second,
		now.Add(-time.Hour).Format(http.TimeFormat):       0,
	}
	for in, want := range cases {
		if got := parseRetryAfter(in, now); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestSendRetriesTransientOnlyWithDedupeKey(t *testing.T) {
	var hits atomic.Int32
	var keys []string
	handler := func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		if hits.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(201)
	}
	opts := []resilient.Option{resilient.WithMaxAttempts(2), resilient.WithBackoff(time.Millisecond, time.Millisecond), resilient.WithTimeout(5 * time.Second)}

	s, sub, key, _ := newTarget(t, handler, opts...)
	m := msg()
	m.DedupeKey = "evt-1"
	res, err := s.Send(context.Background(), sub, key.KID, m)
	if err != nil || res.Outcome != Delivered || hits.Load() != 2 {
		t.Fatalf("with DedupeKey: %+v err=%v hits=%d", res, err, hits.Load())
	}
	if len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] {
		t.Errorf("Idempotency-Key across attempts = %v", keys)
	}
	if keys[0] == sub.Endpoint {
		t.Error("the endpoint (a secret) must not travel as the key")
	}

	hits.Store(0)
	keys = nil
	s2, sub2, key2, _ := newTarget(t, handler, opts...)
	res, err = s2.Send(context.Background(), sub2, key2.KID, msg())
	if err != nil || res.Outcome != Transient || hits.Load() != 1 {
		t.Errorf("without DedupeKey: %+v err=%v hits=%d (want one attempt)", res, err, hits.Load())
	}
}

func TestSendBreakerOpensPerHost(t *testing.T) {
	var hits atomic.Int32
	s, sub, key, _ := newTarget(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(503)
	}, resilient.WithMaxAttempts(1), resilient.WithBreaker(resilient.Config{Failures: 2, Cooldown: time.Minute}))
	var last error
	for i := 0; i < 5; i++ {
		_, last = s.Send(context.Background(), sub, key.KID, msg())
	}
	if hits.Load() != 2 {
		t.Errorf("server hit %d times, want 2 (circuit opens after 2 failures)", hits.Load())
	}
	if !errors.Is(last, resilient.ErrCircuitOpen) {
		t.Errorf("last error = %v, want ErrCircuitOpen", last)
	}
}

func TestSendTimeoutAndCancel(t *testing.T) {
	s, sub, key, srv := newTarget(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	_ = srv
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	res, err := s.Send(ctx, sub, key.KID, msg())
	if err == nil || res.Outcome != Transient {
		t.Errorf("timeout: %+v err=%v", res, err)
	}
}

// The default client must never reach a private address, even for an endpoint that passed ParseSubscription
// by DNS tricks: the dial itself is refused.
func TestNewClientRefusesLoopback(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	_, key, _ := GenerateKey()
	sub, _ := newTestSubscription(t)
	sub.Endpoint = srv.URL + "/x"
	s := NewSender(NewClient(), "mailto:ops@example.com", key)
	res, err := s.Send(context.Background(), sub, key.KID, msg())
	if err == nil || res.Outcome != Transient || hits.Load() != 0 {
		t.Errorf("loopback reached: %+v err=%v hits=%d", res, err, hits.Load())
	}
}

func TestOutcomeString(t *testing.T) {
	for o := Delivered; o <= Transient; o++ {
		if o.String() == "unknown" {
			t.Errorf("outcome %d has no name", o)
		}
	}
	if Outcome(0).String() != "unknown" {
		t.Error("zero Outcome must not look valid")
	}
}
