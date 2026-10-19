package deadline_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"gopkg.aoctech.app/api-commons/deadline"
)

func app(t *testing.T, d time.Duration, skip ...func(fiber.Ctx) bool) *fiber.App {
	t.Helper()
	a := fiber.New()
	a.Use(deadline.New(d, skip...))
	a.Get("/fast", func(c fiber.Ctx) error {
		if _, ok := c.Context().Deadline(); !ok {
			return fiber.NewError(fiber.StatusInternalServerError, "no deadline on the request context")
		}
		return c.SendString("ok")
	})
	// Stands for a DynamoDB or Valkey call that honours its context.
	a.Get("/slow", func(c fiber.Ctx) error {
		select {
		case <-c.Context().Done():
			return c.Context().Err()
		case <-time.After(2 * time.Second):
			return c.SendString("finished")
		}
	})
	a.Get("/export", func(c fiber.Ctx) error {
		time.Sleep(80 * time.Millisecond)
		if c.Context().Err() != nil {
			return c.Context().Err()
		}
		return c.SendString("exported")
	})
	return a
}

func status(t *testing.T, a *fiber.App, path string, hdr ...string) int {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := a.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode
}

func TestARequestCarriesTheDeadline(t *testing.T) {
	if got := status(t, app(t, time.Second), "/fast"); got != 200 {
		t.Fatalf("status %d, want 200", got)
	}
}

func TestAnOverrunBecomes504AndEndsTheWork(t *testing.T) {
	start := time.Now()
	if got := status(t, app(t, 50*time.Millisecond), "/slow"); got != fiber.StatusGatewayTimeout {
		t.Fatalf("status %d, want 504", got)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("the call ran %s: the deadline did not end it", time.Since(start))
	}
}

func TestASkippedRouteIsNotBounded(t *testing.T) {
	a := app(t, 20*time.Millisecond, func(c fiber.Ctx) bool { return c.Path() == "/export" })
	if got := status(t, a, "/export"); got != 200 {
		t.Fatalf("status %d, want 200 on a skipped route", got)
	}
}

func TestAWebSocketUpgradeIsNeverBounded(t *testing.T) {
	a := fiber.New()
	a.Use(deadline.New(time.Millisecond))
	a.Get("/ws", func(c fiber.Ctx) error {
		if _, ok := c.Context().Deadline(); ok {
			return context.DeadlineExceeded
		}
		return c.SendString("upgrade")
	})
	if got := status(t, a, "/ws", "Upgrade", "WebSocket"); got != 200 {
		t.Fatalf("status %d: an upgrade request got a deadline", got)
	}
}
