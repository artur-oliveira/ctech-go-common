package problem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

func serve(t *testing.T, err error) (*http.Response, map[string]any) {
	t.Helper()
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Get("/x", func(fiber.Ctx) error { return err })
	resp, e := app.Test(httptest.NewRequest(http.MethodGet, "/x", nil))
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := io.ReadAll(resp.Body)
	body := map[string]any{}
	if e := json.Unmarshal(raw, &body); e != nil {
		t.Fatalf("body %q is not JSON: %v", raw, e)
	}
	return resp, body
}

func TestErrorHandler(t *testing.T) {
	secret := errors.New("dynamo: table ctech-prod-x key PK#123 failed")
	tests := []struct {
		name       string
		err        error
		status     int
		typ        string
		wantDetail string // "" = don't check
	}{
		{"problem", NotFound("no such thing"), 404, TypeNotFound, "no such thing"},
		{"wrapped problem", fmt.Errorf("svc: %w", Conflict("dup")), 409, TypeConflict, "dup"},
		{"fiber 404", fiber.ErrNotFound, 404, TypeNotFound, ""},
		{"fiber 400", fiber.NewError(400, "bad json"), 400, TypeBadRequest, "bad json"},
		{"fiber 413", fiber.ErrRequestEntityTooLarge, 413, TypeHTTPError, ""},
		{"fiber 429", fiber.ErrTooManyRequests, 429, TypeTooManyRequests, ""},
		{"fiber 504", fiber.ErrGatewayTimeout, 504, TypeTimeout, detailTimeout},
		{"fiber 503", fiber.ErrServiceUnavailable, 503, TypeServiceUnavailable, ""},
		{"fiber 500 does not leak", fiber.NewError(500, secret.Error()), 500, TypeInternalServer, detailInternal},
		{"deadline", fmt.Errorf("query: %w", context.DeadlineExceeded), 504, TypeTimeout, detailTimeout},
		{"unknown does not leak", secret, 500, TypeInternalServer, detailInternal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := serve(t, tc.err)
			if resp.StatusCode != tc.status {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			if ct := resp.Header.Get(fiber.HeaderContentType); ct != ContentType {
				t.Errorf("content-type = %q", ct)
			}
			if body["type"] != tc.typ {
				t.Errorf("type = %v, want %s", body["type"], tc.typ)
			}
			if tc.wantDetail != "" && body["detail"] != tc.wantDetail {
				t.Errorf("detail = %v, want %q", body["detail"], tc.wantDetail)
			}
			if raw, _ := json.Marshal(body); strings.Contains(string(raw), "PK#123") {
				t.Errorf("internal cause leaked: %s", raw)
			}
		})
	}
}

func TestFromErrorNil(t *testing.T) {
	if FromError(nil) != nil {
		t.Fatal("FromError(nil) must be nil")
	}
}

func TestFromErrorKeepsCause(t *testing.T) {
	cause := errors.New("boom")
	if got := FromError(cause).Cause(); got != cause {
		t.Fatalf("Cause() = %v", got)
	}
}

func TestSendRetryAfterHeaderMatchesBody(t *testing.T) {
	resp, body := serve(t, TooManyRequestsAfter("slow down", 30))
	if got := resp.Header.Get(fiber.HeaderRetryAfter); got != "30" {
		t.Errorf("Retry-After = %q", got)
	}
	if body["retry_after_seconds"] != float64(30) || body["next_action"] != NextActionRetry {
		t.Errorf("body = %v", body)
	}
	resp, _ = serve(t, TooManyRequests("no window"))
	if got := resp.Header.Get(fiber.HeaderRetryAfter); got != "" {
		t.Errorf("Retry-After = %q, want none", got)
	}
}

func TestSendExtensions(t *testing.T) {
	p := Forbidden("step up").
		WithCode("step_up_required").
		WithMaxAge(5*time.Minute+900*time.Millisecond).
		WithRequiredScope("wallet:write").
		WithInstance("/x").
		WithNextAction(NextActionReauthenticate, 0).
		WithExtra("blockers", []string{"org"}).
		WithExtra("status", 200) // must not override the typed member
	_, body := serve(t, p)
	want := map[string]any{
		"code": "step_up_required", "max_age_seconds": float64(300), "required_scope": "wallet:write",
		"instance": "/x", "next_action": NextActionReauthenticate, "status": float64(403),
	}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("%s = %v, want %v", k, body[k], v)
		}
	}
	if b, ok := body["blockers"].([]any); !ok || len(b) != 1 || b[0] != "org" {
		t.Errorf("blockers = %v", body["blockers"])
	}
}

func TestSendFieldErrors(t *testing.T) {
	p := UnprocessableEntityFields("invalid", Field("items[0].qty", "too_small", "min 1", "min", 1, 7, "ignored", "odd"))
	_, body := serve(t, p)
	errs := body["errors"].([]any)
	fe := errs[0].(map[string]any)
	if fe["field"] != "items[0].qty" || fe["code"] != "too_small" || fe["message"] != "min 1" {
		t.Errorf("field error = %v", fe)
	}
	if params := fe["params"].(map[string]any); len(params) != 1 || params["min"] != float64(1) {
		t.Errorf("params = %v", params)
	}
}

func TestSendUnmarshalableExtraFallsBackToProblem500(t *testing.T) {
	resp, body := serve(t, BadRequest("x").WithExtra("bad", make(chan int)))
	if resp.StatusCode != 500 || body["type"] != TypeInternalServer {
		t.Fatalf("status %d body %v", resp.StatusCode, body)
	}
	if ct := resp.Header.Get(fiber.HeaderContentType); ct != ContentType {
		t.Errorf("content-type = %q", ct)
	}
}

func TestSendNonErrorStatusBecomes500(t *testing.T) {
	resp, _ := serve(t, New(200, "/problems/x", "OK", ""))
	if resp.StatusCode != 500 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestConstructors(t *testing.T) {
	tests := []struct {
		p      *Problem
		status int
		typ    string
		action string
	}{
		{ServiceUnavailable("d"), 503, TypeServiceUnavailable, NextActionRetry},
		{GatewayTimeout("d"), 504, TypeTimeout, NextActionRetry},
		{NotImplemented("d"), 501, TypeNotImplemented, ""},
		{PayloadTooLarge("d"), 413, TypePayloadTooLarge, ""},
		{PaymentRequired("d"), 402, TypePaymentRequired, ""},
		{PreconditionFailed("d"), 412, TypePreconditionFailed, ""},
		{UnprocessableEntityFields("d"), 422, TypeUnprocessableEntity, ""},
	}
	for _, tc := range tests {
		if tc.p.Status != tc.status || tc.p.Type != tc.typ || tc.p.NextAction != tc.action {
			t.Errorf("%s: got %d %s %q", tc.typ, tc.p.Status, tc.p.Type, tc.p.NextAction)
		}
	}
}

// An embedding wrapper (the shape every product uses today) must keep
// marshalling its own fields: Problem must not define MarshalJSON.
func TestEmbeddingStillMarshalsOuterFields(t *testing.T) {
	type wrapped struct {
		Problem
		Quota int `json:"quota"`
	}
	b, err := json.Marshal(wrapped{Problem: *Conflict("x"), Quota: 3})
	if err != nil || !strings.Contains(string(b), `"quota":3`) {
		t.Fatalf("%s %v", b, err)
	}
}
