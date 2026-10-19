package fiberlimit

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"

	"gopkg.aoctech.app/api-commons/ratelimit"
)

func app(b *ratelimit.Bucket) *fiber.App {
	a := fiber.New()
	a.Use(New(b, func(c fiber.Ctx) string { return c.Get("X-Tenant") }, func(c fiber.Ctx, secs int) error {
		return c.Status(fiber.StatusTooManyRequests).SendString("slow down")
	}))
	a.Get("/", func(c fiber.Ctx) error { return c.SendString("ok") })
	return a
}

func status(t *testing.T, a *fiber.App, tenant string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("GET", "/", nil)
	if tenant != "" {
		req.Header.Set("X-Tenant", tenant)
	}
	res, err := a.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, res.Header.Get("Retry-After")
}

func TestRefusesTheTenantOverItsBudgetAndOnlyThatTenant(t *testing.T) {
	a := app(ratelimit.NewBucket(0.5, 2, 0))
	for i := 0; i < 2; i++ {
		if st, _ := status(t, a, "t1"); st != 200 {
			t.Fatalf("burst request %d: %d", i, st)
		}
	}
	st, retry := status(t, a, "t1")
	if st != 429 || retry != "2" {
		t.Fatalf("over budget at 0.5/s must wait 2s: %d retry-after %q", st, retry)
	}
	if st, _ := status(t, a, "t2"); st != 200 {
		t.Fatalf("another tenant is unaffected: %d", st)
	}
	if st, _ := status(t, a, ""); st != 200 {
		t.Fatalf("no identity is not limited: %d", st)
	}
}

func TestANilBucketIsAPassThrough(t *testing.T) {
	a := app(nil)
	for i := 0; i < 50; i++ {
		if st, _ := status(t, a, "t1"); st != 200 {
			t.Fatalf("request %d: %d", i, st)
		}
	}
}
