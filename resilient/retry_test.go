package resilient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.aoctech.app/api-commons/observability"
)

// script answers each call from a list of statuses (the last repeats), with
// optional headers, and records the requests it saw.
type script struct {
	calls    atomic.Int32
	statuses []int
	err      error
	header   http.Header
	body     string
	seen     []*http.Request
	bodies   []string
}

func (s *script) RoundTrip(req *http.Request) (*http.Response, error) {
	i := int(s.calls.Add(1)) - 1
	s.seen = append(s.seen, req)
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		s.bodies = append(s.bodies, string(b))
	}
	if s.err != nil {
		return nil, s.err
	}
	st := s.statuses[min(i, len(s.statuses)-1)]
	h := http.Header{}
	for k, v := range s.header {
		h[k] = v
	}
	return &http.Response{StatusCode: st, Header: h, Body: io.NopCloser(strings.NewReader(s.body)), Request: req}, nil
}

type sleeps []time.Duration

func (s *sleeps) fn(_ context.Context, d time.Duration) error { *s = append(*s, d); return nil }

func newTestRT(s *script, sl *sleeps, opts ...Option) http.RoundTripper {
	all := append([]Option{
		WithBaseTransport(s), WithoutBreaker(),
		func(o *options) { o.sleep = sl.fn; o.jitter = func(c time.Duration) time.Duration { return c } },
	}, opts...)
	return NewRetryTransport(all...)
}

func do(rt http.RoundTripper, method, body string, hdr map[string]string) (*http.Response, error) {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, "http://svc.test/v1/x", r)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return rt.RoundTrip(req)
}

func TestRetryDecision(t *testing.T) {
	key := map[string]string{HeaderIdempotencyKey: "k1"}
	tests := []struct {
		name      string
		method    string
		hdr       map[string]string
		statuses  []int
		wantCalls int32
		wantCode  int
	}{
		{"GET 503 then 200 retries", "GET", nil, []int{503, 200}, 2, 200},
		{"GET 500 retries until spent", "GET", nil, []int{500}, 3, 500},
		{"GET 404 never retried", "GET", nil, []int{404}, 1, 404},
		{"GET 429 never retried", "GET", nil, []int{429}, 1, 429},
		{"GET 408 never retried", "GET", nil, []int{408}, 1, 408},
		{"GET 501 not retried", "GET", nil, []int{501}, 1, 501},
		{"POST 503 without key not retried", "POST", nil, []int{503, 200}, 1, 503},
		{"POST 503 with key retried", "POST", key, []int{503, 200}, 2, 200},
		{"PUT without key not retried", "PUT", nil, []int{503, 200}, 1, 503},
		{"POST with key 400 not retried", "POST", key, []int{400}, 1, 400},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &script{statuses: tc.statuses}
			var sl sleeps
			resp, err := do(newTestRT(s, &sl), tc.method, "", tc.hdr)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tc.wantCode || s.calls.Load() != tc.wantCalls {
				t.Fatalf("code=%d calls=%d, want %d/%d", resp.StatusCode, s.calls.Load(), tc.wantCode, tc.wantCalls)
			}
		})
	}
}

func TestTransportErrorRetriedOnlyWhenSafe(t *testing.T) {
	boom := errors.New("connection reset")
	for _, tc := range []struct {
		method string
		want   int32
	}{{"GET", 3}, {"POST", 1}} {
		s := &script{err: boom}
		var sl sleeps
		if _, err := do(newTestRT(s, &sl), tc.method, "", nil); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
		if s.calls.Load() != tc.want {
			t.Fatalf("%s calls = %d, want %d", tc.method, s.calls.Load(), tc.want)
		}
	}
}

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	s := &script{statuses: []int{503}}
	var sl sleeps
	rt := newTestRT(s, &sl, WithMaxAttempts(6), WithBackoff(100*time.Millisecond, 500*time.Millisecond))
	if _, err := do(rt, "GET", "", nil); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{100, 200, 400, 500, 500}
	if len(sl) != len(want) {
		t.Fatalf("sleeps = %v", sl)
	}
	for i, w := range want {
		if sl[i] != w*time.Millisecond {
			t.Fatalf("sleep[%d] = %v, want %v", i, sl[i], w*time.Millisecond)
		}
	}
}

func TestRetryAfter(t *testing.T) {
	tests := []struct {
		name       string
		header     string
		wantCalls  int32
		wantSleeps int
		wantFirst  time.Duration
	}{
		{"seconds honored", "1", 2, 1, time.Second},
		{"zero means now", "0", 2, 1, 0},
		{"over cap is not waited for", "30", 1, 0, 0},
		{"HTTP date in the past", time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat), 2, 1, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &script{statuses: []int{503, 200}, header: http.Header{"Retry-After": {tc.header}}}
			var sl sleeps
			if _, err := do(newTestRT(s, &sl), "GET", "", nil); err != nil {
				t.Fatal(err)
			}
			if s.calls.Load() != tc.wantCalls || len(sl) != tc.wantSleeps {
				t.Fatalf("calls=%d sleeps=%v", s.calls.Load(), sl)
			}
			if tc.wantSleeps > 0 && sl[0] != tc.wantFirst {
				t.Fatalf("sleep = %v, want %v", sl[0], tc.wantFirst)
			}
		})
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"", 0, false}, {"abc", 0, false}, {"-5", 0, false}, {" 3 ", 3 * time.Second, true},
		{now.Add(10 * time.Second).Format(http.TimeFormat), 10 * time.Second, true},
	}
	for _, tc := range tests {
		got, ok := parseRetryAfter(tc.in, now)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%q = %v,%v want %v,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestBodyReplayedOnRetry(t *testing.T) {
	s := &script{statuses: []int{503, 200}}
	var sl sleeps
	hdr := map[string]string{HeaderIdempotencyKey: "k"}
	if _, err := do(newTestRT(s, &sl), "POST", "payload", hdr); err != nil {
		t.Fatal(err)
	}
	if len(s.bodies) != 2 || s.bodies[0] != "payload" || s.bodies[1] != "payload" {
		t.Fatalf("bodies = %q", s.bodies)
	}
}

func TestNonReplayableBodySentOnce(t *testing.T) {
	s := &script{statuses: []int{503, 200}}
	var sl sleeps
	req, _ := http.NewRequest("POST", "http://svc.test/x", io.NopCloser(strings.NewReader("p"))) // no GetBody
	req.Header.Set(HeaderIdempotencyKey, "k")
	resp, err := newTestRT(s, &sl).RoundTrip(req)
	if err != nil || resp.StatusCode != 503 || s.calls.Load() != 1 {
		t.Fatalf("resp=%v err=%v calls=%d", resp, err, s.calls.Load())
	}
}

func TestContextCancelledStopsRetry(t *testing.T) {
	s := &script{statuses: []int{503}}
	ctx, cancel := context.WithCancel(context.Background())
	var sl sleeps
	rt := newTestRT(s, &sl, func(o *options) {
		o.sleep = func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }
	})
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://svc.test/x", nil)
	if _, err := rt.RoundTrip(req); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if s.calls.Load() != 1 {
		t.Fatalf("calls = %d", s.calls.Load())
	}
}

func TestMaxAttemptsOneDisablesRetry(t *testing.T) {
	s := &script{statuses: []int{503}}
	var sl sleeps
	if _, err := do(newTestRT(s, &sl, WithMaxAttempts(1)), "GET", "", nil); err != nil || s.calls.Load() != 1 {
		t.Fatalf("calls = %d err = %v", s.calls.Load(), err)
	}
}

func TestHeaderInjection(t *testing.T) {
	s := &script{statuses: []int{200}}
	var sl sleeps
	rt := newTestRT(s, &sl, WithIdempotencyKeyFunc(func(r *http.Request) string {
		return DeterministicKey("op", r.URL.Path)
	}))
	ctx := observability.WithRequestID(context.Background(), "rid-1")
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://svc.test/v1/x", nil)
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	got := s.seen[0].Header
	if got.Get(HeaderRequestID) != "rid-1" || got.Get(HeaderIdempotencyKey) != DeterministicKey("op", "/v1/x") {
		t.Fatalf("headers = %v", got)
	}
	if req.Header.Get(HeaderIdempotencyKey) != "" || req.Header.Get(HeaderRequestID) != "" {
		t.Fatal("caller's request was modified")
	}

	// An injected key makes a POST retryable; a GET gets none; a caller's header wins.
	s2 := &script{statuses: []int{503, 200}}
	rt = newTestRT(s2, &sl, WithIdempotencyKeyFunc(func(*http.Request) string { return "gen" }))
	if resp, _ := do(rt, "POST", "", nil); resp.StatusCode != 200 || s2.calls.Load() != 2 {
		t.Fatalf("POST with injected key: calls = %d", s2.calls.Load())
	}
	do(rt, "GET", "", nil)
	if s2.seen[len(s2.seen)-1].Header.Get(HeaderIdempotencyKey) != "" {
		t.Fatal("GET got a key")
	}
	do(rt, "POST", "", map[string]string{HeaderIdempotencyKey: "mine", HeaderRequestID: "r"})
	if h := s2.seen[len(s2.seen)-1].Header; h.Get(HeaderIdempotencyKey) != "mine" || h.Get(HeaderRequestID) != "r" {
		t.Fatalf("caller header overridden: %v", h)
	}
}

func TestDeterministicKey(t *testing.T) {
	if DeterministicKey("a", "b") != DeterministicKey("a", "b") {
		t.Fatal("not deterministic")
	}
	if DeterministicKey("ab", "c") == DeterministicKey("a", "bc") {
		t.Fatal("parts run together")
	}
	if len(DeterministicKey("x")) != 64 {
		t.Fatal("length")
	}
}

func TestBreakerEndsRetriesAndCountsEveryAttempt(t *testing.T) {
	s := &script{statuses: []int{503}}
	var sl sleeps
	rt := NewRetryTransport(WithBaseTransport(s), WithMaxAttempts(5),
		WithBreaker(Config{Failures: 2, OnChange: func(string, State, State) {}}),
		func(o *options) { o.sleep = sl.fn })
	_, err := do(rt, "GET", "", nil)
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("err = %v, want ErrCircuitOpen", err)
	}
	if s.calls.Load() != 2 {
		t.Fatalf("host calls = %d, want 2 (circuit opens, then no more)", s.calls.Load())
	}
}

func TestPerEndpointCircuit(t *testing.T) {
	s := &script{statuses: []int{503}}
	var sl sleeps
	rt := NewRetryTransport(WithBaseTransport(s), WithMaxAttempts(1), WithPerEndpointCircuit(),
		WithBreaker(Config{Failures: 1, OnChange: func(string, State, State) {}}),
		func(o *options) { o.sleep = sl.fn })
	call := func(path string) error {
		req, _ := http.NewRequest("GET", "http://svc.test"+path, nil)
		_, err := rt.RoundTrip(req)
		return err
	}
	_ = call("/a") // opens /a
	if err := call("/a"); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("/a err = %v", err)
	}
	if err := call("/b"); errors.Is(err, ErrCircuitOpen) {
		t.Fatal("/b was cut off by /a")
	}
}

func TestNegativeCache(t *testing.T) {
	s := &script{statuses: []int{404}, body: "nope"}
	var sl sleeps
	rt := newTestRT(s, &sl, WithNegativeCache(time.Minute, http.StatusNotFound))
	get := func(auth string) (*http.Response, string) {
		hdr := map[string]string{}
		if auth != "" {
			hdr[headerAuthorization] = auth
		}
		resp, err := do(rt, "GET", "", hdr)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	for range 3 {
		if resp, body := get("Bearer a"); resp.StatusCode != 404 || body != "nope" {
			t.Fatalf("resp = %d %q", resp.StatusCode, body)
		}
	}
	if s.calls.Load() != 1 {
		t.Fatalf("host calls = %d, want 1", s.calls.Load())
	}
	get("Bearer b") // another caller is not served a's answer
	if s.calls.Load() != 2 {
		t.Fatalf("host calls = %d, want 2", s.calls.Load())
	}
	do(rt, "POST", "", nil) // writes are never cached
	do(rt, "POST", "", nil)
	if s.calls.Load() != 4 {
		t.Fatalf("host calls = %d, want 4", s.calls.Load())
	}
}

func TestNegativeCacheIgnoresOtherStatuses(t *testing.T) {
	s := &script{statuses: []int{200}}
	var sl sleeps
	rt := newTestRT(s, &sl, WithNegativeCache(0, http.StatusNotFound))
	do(rt, "GET", "", nil)
	do(rt, "GET", "", nil)
	if s.calls.Load() != 2 {
		t.Fatalf("calls = %d", s.calls.Load())
	}
}

func TestNegativeCacheOversizeBodyPassesThrough(t *testing.T) {
	s := &script{statuses: []int{404}, body: strings.Repeat("x", maxNegativeBodyBytes+10)}
	var sl sleeps
	rt := newTestRT(s, &sl, WithNegativeCache(time.Minute, 404))
	for range 2 {
		resp, _ := do(rt, "GET", "", nil)
		b, _ := io.ReadAll(resp.Body)
		if len(b) != maxNegativeBodyBytes+10 {
			t.Fatalf("body len = %d", len(b))
		}
	}
	if s.calls.Load() != 2 {
		t.Fatalf("oversize answer was cached: calls = %d", s.calls.Load())
	}
}

func TestNewHTTPClientDefaults(t *testing.T) {
	c := NewHTTPClient(WithTimeout(3*time.Second), WithDialTimeouts(time.Second, 2*time.Second, 4*time.Second))
	if c.Timeout != 3*time.Second {
		t.Fatalf("timeout = %v", c.Timeout)
	}
	if c.Transport == nil {
		t.Fatal("no transport")
	}
	if NewHTTPClient().Timeout != DefaultTimeout {
		t.Fatal("default timeout")
	}
}

// End to end over a real socket: one 503 with Retry-After: 0, then success.
func TestNewHTTPClientAgainstRealServer(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, r.Header.Get(HeaderRequestID))
	}))
	defer srv.Close()
	c := NewHTTPClient()
	ctx := observability.WithRequestID(context.Background(), "rid-9")
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(b) != "rid-9" || n.Load() != 2 {
		t.Fatalf("code=%d body=%q calls=%d", resp.StatusCode, b, n.Load())
	}
}
