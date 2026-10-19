package fiber

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	fiberapi "github.com/gofiber/fiber/v3"
)

func runAccessLog(t *testing.T, cfg AccessLogConfig, path string, hdr map[string]string) (string, int) {
	t.Helper()
	var buf bytes.Buffer
	cfg.Output = &buf
	app := fiberapi.New()
	app.Use(AccessLog(cfg)) // mounted BEFORE RequestID on purpose
	app.Use(RequestID())
	h := func(c fiberapi.Ctx) error { return c.SendString("ok") }
	app.Get("/v1/x", h)
	app.Get("/v1/health", h)
	app.Get("/v1.0/health-check", h)
	app.Get("/boom", func(c fiberapi.Ctx) error { return errors.New("secret detail") })
	app.Get("/teapot", func(c fiberapi.Ctx) error { return fiberapi.NewError(418) })
	req := httptest.NewRequest("GET", path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return buf.String(), resp.StatusCode
}

func TestAccessLog(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		hdr        map[string]string
		wantLine   bool
		wantStatus int
		wantRID    string
	}{
		{"ok generates request id", "/v1/x", nil, true, 200, ""},
		{"keeps caller request id", "/v1/x", map[string]string{RequestIDHeader: "rid-1"}, true, 200, "rid-1"},
		{"skips health", "/v1/health", nil, false, 200, ""},
		{"skips health-check", "/v1.0/health-check", nil, false, 200, ""},
		{"error logs real status", "/boom", nil, true, 500, ""},
		{"fiber error status", "/teapot", nil, true, 418, ""},
		{"query not logged", "/v1/x?token=abc", nil, true, 200, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, status := runAccessLog(t, AccessLogConfig{}, tc.path, tc.hdr)
			if !tc.wantLine {
				if out != "" {
					t.Fatalf("logged: %q", out)
				}
				return
			}
			if strings.Contains(out, "\x1b[") || strings.Contains(out, "token") || strings.Contains(out, "secret") {
				t.Fatalf("leak or escape in %q", out)
			}
			var l accessLine
			if err := json.Unmarshal([]byte(out), &l); err != nil {
				t.Fatalf("not JSON: %v: %q", err, out)
			}
			if l.Status != tc.wantStatus || status != tc.wantStatus || l.Msg != AccessLogMessage || l.Latency == "" {
				t.Errorf("line %+v, response status %d", l, status)
			}
			if l.RequestID == "" || (tc.wantRID != "" && l.RequestID != tc.wantRID) {
				t.Errorf("request_id = %q", l.RequestID)
			}
		})
	}
}

func TestAccessLogRedactsSensitiveHeaders(t *testing.T) {
	out, _ := runAccessLog(t, AccessLogConfig{Headers: []string{"Authorization", "User-Agent", "cookie"}}, "/v1/x",
		map[string]string{"Authorization": "Bearer s3cr3t", "User-Agent": "ua", "Cookie": "sid=1"})
	if strings.Contains(out, "s3cr3t") || strings.Contains(out, "sid=1") {
		t.Fatalf("secret logged: %q", out)
	}
	var l accessLine
	if err := json.Unmarshal([]byte(out), &l); err != nil {
		t.Fatal(err)
	}
	if l.Headers["Authorization"] != RedactedValue || l.Headers["Cookie"] != RedactedValue || l.Headers["User-Agent"] != "ua" {
		t.Errorf("headers = %v", l.Headers)
	}
}
