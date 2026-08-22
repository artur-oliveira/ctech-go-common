package fiber

import (
	"errors"
	"net/http/httptest"
	"testing"

	fiberapi "github.com/gofiber/fiber/v3"
	"gopkg.aoctech.app/api-commons/observability"
)

func TestRequestIDPreservesValidHeaderAndAttachesContext(t *testing.T) {
	app := fiberapi.New()
	app.Use(RequestID())
	app.Get("/", func(c fiberapi.Ctx) error {
		return c.SendString(observability.RequestID(c.Context()))
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(RequestIDHeader, "req-123")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get(RequestIDHeader); got != "req-123" {
		t.Fatalf("response request ID = %q", got)
	}
}

func TestRequestIDReplacesUnsafeHeaderAndStoresLocal(t *testing.T) {
	app := fiberapi.New()
	app.Use(RequestID(RequestIDConfig{LocalsKey: "request_id", Generator: func() string { return "generated-1" }}))
	app.Get("/", func(c fiberapi.Ctx) error {
		if got, _ := c.Locals("request_id").(string); got != "generated-1" {
			t.Fatalf("local request ID = %q", got)
		}
		return nil
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(RequestIDHeader, "contains space")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get(RequestIDHeader); got != "generated-1" {
		t.Fatalf("generated request ID = %q", got)
	}
}

func TestRequestIDReplacesUnsafeGeneratedValue(t *testing.T) {
	app := fiberapi.New()
	app.Use(RequestID(RequestIDConfig{Generator: func() string { return "contains space" }}))
	app.Get("/", func(c fiberapi.Ctx) error { return nil })

	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get(RequestIDHeader); !validRequestID(got) {
		t.Fatalf("fallback request ID = %q, want a valid value", got)
	}
}

func TestLogHTTPErrorAcceptsCause(t *testing.T) {
	app := fiberapi.New()
	app.Use(RequestID(RequestIDConfig{Generator: func() string { return "req-test" }}))
	app.Get("/", func(c fiberapi.Ctx) error {
		LogHTTPError(c, fiberapi.StatusInternalServerError, "/problems/internal", errors.New("offline"))
		return nil
	})
	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
}
