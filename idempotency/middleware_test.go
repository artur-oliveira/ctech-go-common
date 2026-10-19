package idempotency_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"gopkg.aoctech.app/api-commons/idempotency"
)

const scopeLocal = "scope"

type harness struct {
	app   *fiber.App
	calls atomic.Int32
	reply func(c fiber.Ctx) error
}

func newHarness(t *testing.T, cfg idempotency.Config) *harness {
	t.Helper()
	h := &harness{}
	h.reply = func(c fiber.Ctx) error { return c.Status(http.StatusCreated).JSON(fiber.Map{"n": h.calls.Load()}) }
	if cfg.Store == nil {
		cfg.Store = &idempotency.MemoryStore{}
	}
	if cfg.Scope == nil {
		cfg.Scope = idempotency.ScopeFromLocals(scopeLocal)
	}
	h.app = fiber.New()
	h.app.Use(func(c fiber.Ctx) error {
		if t := c.Get("X-Tenant"); t != "" {
			c.Locals(scopeLocal, t)
		}
		return c.Next()
	})
	h.app.Use(idempotency.New(cfg))
	h.app.All("/pay/:id", func(c fiber.Ctx) error {
		h.calls.Add(1)
		return h.reply(c)
	})
	return h
}

type resp struct {
	status int
	body   string
	hdr    http.Header
}

func (h *harness) do(t *testing.T, method, path, body string, hdr map[string]string) resp {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Tenant", "t1")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := h.app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{res.StatusCode, string(b), res.Header}
}

func key(k string) map[string]string { return map[string]string{idempotency.HeaderKey: k} }

func TestReplayReturnsTheStoredAnswer(t *testing.T) {
	h := newHarness(t, idempotency.Config{})
	first := h.do(t, "POST", "/pay/1", `{"a":1}`, key("k1"))
	second := h.do(t, "POST", "/pay/1", `{"a":1}`, key("k1"))
	if first.status != 201 || second.status != 201 || first.body != second.body {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	if first.hdr.Get(idempotency.HeaderReplay) != "" || second.hdr.Get(idempotency.HeaderReplay) != "true" {
		t.Fatalf("replay headers: %q / %q", first.hdr.Get(idempotency.HeaderReplay), second.hdr.Get(idempotency.HeaderReplay))
	}
	if second.hdr.Get("Content-Type") != first.hdr.Get("Content-Type") {
		t.Fatalf("content type lost on replay")
	}
	if n := h.calls.Load(); n != 1 {
		t.Fatalf("handler ran %d times, want 1", n)
	}
}

func TestSameKeyDifferentRequestIsAConflict(t *testing.T) {
	cases := map[string]struct{ method, path, body string }{
		"body":   {"POST", "/pay/1", `{"a":2}`},
		"path":   {"POST", "/pay/2", `{"a":1}`},
		"method": {"PUT", "/pay/1", `{"a":1}`},
		"query":  {"POST", "/pay/1?x=1", `{"a":1}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, idempotency.Config{})
			h.do(t, "POST", "/pay/1", `{"a":1}`, key("k"))
			r := h.do(t, tc.method, tc.path, tc.body, key("k"))
			if r.status != 409 || !strings.Contains(r.body, idempotency.TypeConflict) {
				t.Fatalf("got %d %s", r.status, r.body)
			}
			if r.hdr.Get("Content-Type") != "application/problem+json" {
				t.Fatalf("content type %q", r.hdr.Get("Content-Type"))
			}
			if h.calls.Load() != 1 {
				t.Fatalf("handler ran again")
			}
		})
	}
}

func TestScopeIsolatesKeys(t *testing.T) {
	h := newHarness(t, idempotency.Config{})
	h.do(t, "POST", "/pay/1", `{}`, key("k"))
	r := h.do(t, "POST", "/pay/1", `{}`, map[string]string{idempotency.HeaderKey: "k", "X-Tenant": "t2"})
	if r.hdr.Get(idempotency.HeaderReplay) != "" || h.calls.Load() != 2 {
		t.Fatalf("tenant t2 got t1's answer (calls=%d)", h.calls.Load())
	}
}

func TestUnresolvedScopeIs500(t *testing.T) {
	h := newHarness(t, idempotency.Config{})
	r := h.do(t, "POST", "/pay/1", `{}`, map[string]string{idempotency.HeaderKey: "k", "X-Tenant": ""})
	// X-Tenant="" leaves the local unset.
	if r.status != 500 || !strings.Contains(r.body, idempotency.TypeScopeUnresolved) || h.calls.Load() != 0 {
		t.Fatalf("got %d %s", r.status, r.body)
	}
}

func TestKeyValidationAndRequirement(t *testing.T) {
	cases := []struct {
		name     string
		required bool
		method   string
		hdr      map[string]string
		want     int
		typ      string
	}{
		{"missing, required", true, "POST", nil, 400, idempotency.TypeKeyRequired},
		{"missing, optional runs", false, "POST", nil, 201, ""},
		{"too long", false, "POST", key(strings.Repeat("a", idempotency.MaxKeyLength+1)), 400, idempotency.TypeKeyInvalid},
		{"max length ok", false, "POST", key(strings.Repeat("a", idempotency.MaxKeyLength)), 201, ""},
		{"non-ascii", false, "POST", key("chave-ç"), 400, idempotency.TypeKeyInvalid},
		{"space", false, "POST", key("a b"), 400, idempotency.TypeKeyInvalid},
		{"GET passes even if required", true, "GET", nil, 201, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, idempotency.Config{Required: tc.required})
			r := h.do(t, tc.method, "/pay/1", `{}`, tc.hdr)
			if r.status != tc.want || (tc.typ != "" && !strings.Contains(r.body, tc.typ)) {
				t.Fatalf("got %d %s", r.status, r.body)
			}
		})
	}
}

func TestOnlyFinalAnswersAreRecorded(t *testing.T) {
	cases := []struct {
		status   int
		recorded bool
	}{
		{200, true}, {201, true}, {204, true}, {400, true}, {404, true}, {422, true},
		{401, false}, {403, false}, {408, false}, {409, false}, {423, false}, {425, false}, {429, false},
		{500, false}, {502, false}, {503, false},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			h := newHarness(t, idempotency.Config{})
			h.reply = func(c fiber.Ctx) error { return c.Status(tc.status).SendString("x") }
			h.do(t, "POST", "/pay/1", `{}`, key("k"))
			second := h.do(t, "POST", "/pay/1", `{}`, key("k"))
			replayed := second.hdr.Get(idempotency.HeaderReplay) == "true"
			if replayed != tc.recorded {
				t.Fatalf("status %d: replayed=%v, want %v", tc.status, replayed, tc.recorded)
			}
			if !tc.recorded && h.calls.Load() != 2 {
				t.Fatalf("claim not released: handler ran %d times", h.calls.Load())
			}
		})
	}
}

func TestHandlerErrorReleasesTheClaim(t *testing.T) {
	h := newHarness(t, idempotency.Config{})
	h.reply = func(fiber.Ctx) error { return errors.New("boom") }
	h.do(t, "POST", "/pay/1", `{}`, key("k"))
	h.reply = func(c fiber.Ctx) error { return c.SendString("ok") }
	r := h.do(t, "POST", "/pay/1", `{}`, key("k"))
	if r.status != 200 || r.hdr.Get(idempotency.HeaderReplay) != "" {
		t.Fatalf("retry after handler error: %+v", r)
	}
}

func TestOversizedAnswerIsServedButNotRecorded(t *testing.T) {
	h := newHarness(t, idempotency.Config{MaxBodyBytes: 4})
	h.reply = func(c fiber.Ctx) error { return c.SendString("0123456789") }
	first := h.do(t, "POST", "/pay/1", `{}`, key("k"))
	h.do(t, "POST", "/pay/1", `{}`, key("k"))
	if first.body != "0123456789" || h.calls.Load() != 2 {
		t.Fatalf("body=%q calls=%d", first.body, h.calls.Load())
	}
}

func TestInFlightRequestIsRefusedWithRetryAfter(t *testing.T) {
	h := newHarness(t, idempotency.Config{})
	started, release := make(chan struct{}), make(chan struct{})
	h.reply = func(c fiber.Ctx) error {
		close(started)
		<-release
		return c.SendString("done")
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.do(t, "POST", "/pay/1", `{}`, key("k"))
	}()
	<-started
	r := h.do(t, "POST", "/pay/1", `{}`, key("k"))
	close(release)
	wg.Wait()
	if r.status != 409 || !strings.Contains(r.body, idempotency.TypeInProgress) || r.hdr.Get("Retry-After") == "" {
		t.Fatalf("got %d %s retry-after=%q", r.status, r.body, r.hdr.Get("Retry-After"))
	}
	if after := h.do(t, "POST", "/pay/1", `{}`, key("k")); after.hdr.Get(idempotency.HeaderReplay) != "true" {
		t.Fatalf("retry after completion is not a replay: %+v", after)
	}
}

func TestConcurrentRequestsRunTheHandlerOnce(t *testing.T) {
	h := newHarness(t, idempotency.Config{})
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); h.do(t, "POST", "/pay/1", `{}`, key("k")) }()
	}
	wg.Wait()
	if n := h.calls.Load(); n != 1 {
		t.Fatalf("handler ran %d times, want 1", n)
	}
}

type brokenStore struct{ idempotency.MemoryStore }

func (*brokenStore) Claim(context.Context, string, string, string, time.Duration) (*idempotency.Record, error) {
	return nil, errors.New("store down")
}

func TestStoreOutage(t *testing.T) {
	t.Run("fail closed refuses with 503", func(t *testing.T) {
		h := newHarness(t, idempotency.Config{Store: &brokenStore{}})
		r := h.do(t, "POST", "/pay/1", `{}`, key("k"))
		if r.status != 503 || r.hdr.Get("Retry-After") == "" || h.calls.Load() != 0 {
			t.Fatalf("got %d calls=%d", r.status, h.calls.Load())
		}
	})
	t.Run("fail open runs the handler", func(t *testing.T) {
		h := newHarness(t, idempotency.Config{Store: &brokenStore{}, FailOpen: true})
		r := h.do(t, "POST", "/pay/1", `{}`, key("k"))
		if r.status != 201 || h.calls.Load() != 1 {
			t.Fatalf("got %d calls=%d", r.status, h.calls.Load())
		}
	})
}

func TestExpiredAnswerRunsAgain(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	store := &idempotency.MemoryStore{Now: func() time.Time { return now }}
	h := newHarness(t, idempotency.Config{Store: store, TTL: time.Hour})
	h.do(t, "POST", "/pay/1", `{}`, key("k"))
	now = now.Add(time.Hour + time.Second)
	r := h.do(t, "POST", "/pay/1", `{}`, key("k"))
	if r.hdr.Get(idempotency.HeaderReplay) != "" || h.calls.Load() != 2 {
		t.Fatalf("expired answer replayed (calls=%d)", h.calls.Load())
	}
}

func TestLapsedClaimIsTakenOver(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	store := &idempotency.MemoryStore{Now: func() time.Time { return now }}
	if rec, err := store.Claim(context.Background(), "t1", "k", "h", time.Minute); rec != nil || err != nil {
		t.Fatal(rec, err)
	}
	if _, err := store.Claim(context.Background(), "t1", "k", "h", time.Minute); !errors.Is(err, idempotency.ErrInProgress) {
		t.Fatalf("live claim: %v", err)
	}
	now = now.Add(2 * time.Minute)
	if rec, err := store.Claim(context.Background(), "t1", "k", "h", time.Minute); rec != nil || err != nil {
		t.Fatalf("lapsed claim not taken over: %v %v", rec, err)
	}
}

func TestMethodsAreConfigurable(t *testing.T) {
	h := newHarness(t, idempotency.Config{Methods: []string{"POST"}, Required: true})
	if r := h.do(t, "PUT", "/pay/1", `{}`, nil); r.status != 201 {
		t.Fatalf("PUT should pass through, got %d", r.status)
	}
	if r := h.do(t, "POST", "/pay/1", `{}`, nil); r.status != 400 {
		t.Fatalf("POST without key should be 400, got %d", r.status)
	}
}

func TestNewPanicsOnMisconfiguration(t *testing.T) {
	scope := idempotency.ScopeFromLocals("x")
	cases := map[string]idempotency.Config{
		"no store":     {Scope: scope},
		"no scope":     {Store: &idempotency.MemoryStore{}},
		"negative ttl": {Store: &idempotency.MemoryStore{}, Scope: scope, TTL: -1},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			idempotency.New(cfg)
		})
	}
}
