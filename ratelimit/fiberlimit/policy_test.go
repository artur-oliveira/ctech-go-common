package fiberlimit

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"gopkg.aoctech.app/api-commons/problem"
	"gopkg.aoctech.app/api-commons/ratelimit"
)

type downCounter struct{}

func (downCounter) Incr(context.Context, string, time.Duration) (int64, error) {
	return 0, errors.New("down")
}
func (downCounter) Count(context.Context, string) (int64, error) { return 0, errors.New("down") }

func policyApp(cfg PolicyConfig, handlerStatus int) *fiber.App {
	a := fiber.New()
	a.Use(NewPolicy(cfg))
	a.Get("/", func(c fiber.Ctx) error { return c.SendStatus(handlerStatus) })
	return a
}

func get(t *testing.T, a *fiber.App, key string) (*httptest.ResponseRecorder, []byte) {
	t.Helper()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Key", key)
	res, err := a.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	rec := httptest.NewRecorder()
	rec.Code = res.StatusCode
	for k, v := range res.Header {
		rec.Header()[k] = v
	}
	return rec, body
}

func keyHeader(c fiber.Ctx) string { return c.Get("X-Key") }

func TestPolicy_CountAll_DeniesWithProblemAndHeaders(t *testing.T) {
	lim := &ratelimit.Limiter{Counter: ratelimit.NewMemoryCounter(), Prefix: "t", Max: 2, Window: time.Minute}
	a := policyApp(PolicyConfig{Limiter: lim, Key: keyHeader}, 200)

	rec, _ := get(t, a, "k")
	if rec.Code != 200 || rec.Header().Get(HeaderRateLimitLimit) != "2" || rec.Header().Get(HeaderRateLimitRemaining) != "1" {
		t.Fatalf("1st: %d %v", rec.Code, rec.Header())
	}
	get(t, a, "k")
	rec, body := get(t, a, "k")
	if rec.Code != 429 || rec.Header().Get("Retry-After") == "" || rec.Header().Get(HeaderRateLimitReset) == "" {
		t.Fatalf("3rd: %d %v", rec.Code, rec.Header())
	}
	if ct := rec.Header().Get("Content-Type"); ct != contentTypeProblem {
		t.Fatalf("content-type %q", ct)
	}
	var p problem.Problem
	if err := json.Unmarshal(body, &p); err != nil || p.Type != problem.TypeTooManyRequests || p.RetryAfterSeconds < 1 {
		t.Fatalf("problem %+v %v", p, err)
	}
	if rec, _ := get(t, a, "other"); rec.Code != 200 {
		t.Fatalf("other key limited: %d", rec.Code)
	}
	if rec, _ := get(t, a, ""); rec.Code != 200 {
		t.Fatalf("empty key limited: %d", rec.Code)
	}
}

func TestPolicy_FailModes(t *testing.T) {
	for _, tt := range []struct {
		mode ratelimit.FailMode
		want int
	}{{ratelimit.FailOpen, 200}, {ratelimit.FailClosed, 503}} {
		lim := &ratelimit.Limiter{Counter: downCounter{}, Prefix: "t", Max: 1, Window: time.Minute, FailMode: tt.mode}
		for _, count := range []Count{CountAll, CountFailures} {
			rec, body := get(t, policyApp(PolicyConfig{Limiter: lim, Key: keyHeader, Count: count}, 200), "k")
			if rec.Code != tt.want {
				t.Errorf("%v/%v: status %d want %d", tt.mode, count, rec.Code, tt.want)
			}
			if tt.want == 503 {
				var p problem.Problem
				if json.Unmarshal(body, &p) != nil || p.Type != TypeServiceUnavailable || p.Status != 503 {
					t.Errorf("503 body %s", body)
				}
			}
		}
	}
}

func TestPolicy_CountFailures(t *testing.T) {
	lim := &ratelimit.Limiter{Counter: ratelimit.NewMemoryCounter(), Prefix: "t", Max: 2, Window: time.Minute}
	a := policyApp(PolicyConfig{Limiter: lim, Key: keyHeader, Count: CountFailures}, 401)
	for i := 0; i < 2; i++ {
		if rec, _ := get(t, a, "k"); rec.Code != 401 {
			t.Fatalf("attempt %d: %d", i, rec.Code)
		}
	}
	if rec, _ := get(t, a, "k"); rec.Code != 429 {
		t.Fatalf("expected 429 after 2 failures, got %d", rec.Code)
	}

	ok := policyApp(PolicyConfig{Limiter: lim, Key: keyHeader, Count: CountFailures}, 200)
	for i := 0; i < 5; i++ {
		if rec, _ := get(t, ok, "fresh"); rec.Code != 200 {
			t.Fatalf("successes were counted: %d", rec.Code)
		}
	}
}

func TestPolicy_CountSuccessesAndSkip(t *testing.T) {
	lim := &ratelimit.Limiter{Counter: ratelimit.NewMemoryCounter(), Prefix: "s", Max: 1, Window: time.Minute}
	a := policyApp(PolicyConfig{Limiter: lim, Key: keyHeader, Count: CountSuccesses}, 200)
	get(t, a, "k")
	if rec, _ := get(t, a, "k"); rec.Code != 429 {
		t.Fatalf("success budget not spent: %d", rec.Code)
	}

	lim2 := &ratelimit.Limiter{Counter: ratelimit.NewMemoryCounter(), Prefix: "f", Max: 1, Window: time.Minute}
	b := fiber.New()
	b.Use(NewPolicy(PolicyConfig{Limiter: lim2, Key: keyHeader, Count: CountFailures}))
	b.Get("/", func(c fiber.Ctx) error { SkipCount(c); return c.SendStatus(401) })
	for i := 0; i < 3; i++ {
		if rec, _ := get(t, b, "k"); rec.Code != 401 {
			t.Fatalf("skipped failure counted: %d", rec.Code)
		}
	}
}

func TestPolicy_CustomRenderersAndNilPassThrough(t *testing.T) {
	lim := &ratelimit.Limiter{Counter: ratelimit.NewMemoryCounter(), Prefix: "c", Max: 1, Window: time.Minute}
	a := policyApp(PolicyConfig{Limiter: lim, Key: keyHeader, Deny: func(c fiber.Ctx, secs int) error {
		return c.Status(418).SendString("custom")
	}}, 200)
	get(t, a, "k")
	rec, body := get(t, a, "k")
	if rec.Code != 418 || string(body) != "custom" || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("%d %s %v", rec.Code, body, rec.Header())
	}
	if rec, _ := get(t, policyApp(PolicyConfig{}, 200), "k"); rec.Code != 200 {
		t.Fatalf("nil limiter: %d", rec.Code)
	}
}

func TestIPKey(t *testing.T) {
	ex, _ := ratelimit.NewIPExtractor("0.0.0.0/32") // the peer address fiber's a.Test reports
	a := fiber.New()
	a.Get("/", func(c fiber.Ctx) error { return c.SendString(IPKey(ex)(c)) })
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-For", "198.51.100.7")
	res, err := a.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(res.Body); string(b) != "198.51.100.7" {
		t.Fatalf("got %q", b)
	}
}
