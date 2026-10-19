package fiber

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"

	"gopkg.aoctech.app/api-commons/observability"
)

// Access log field values and limits.
const (
	// AccessLogMessage is the constant `msg` of every access line, the key
	// CloudWatch metric filters select on.
	AccessLogMessage = "http_request"
	// RedactedValue replaces the value of a sensitive header.
	RedactedValue = "[REDACTED]"

	healthSuffix      = "/health"
	healthCheckSuffix = "/health-check"
)

// sensitiveHeaders are never logged verbatim, even when listed in
// AccessLogConfig.Headers.
var sensitiveHeaders = map[string]struct{}{
	"authorization":       {},
	"proxy-authorization": {},
	"cookie":              {},
	"set-cookie":          {},
	"x-api-key":           {},
}

// AccessLogConfig customizes AccessLog. The zero value is production-ready.
type AccessLogConfig struct {
	// Output defaults to os.Stdout. Each request is one Write call.
	Output io.Writer
	// Skip overrides the default, which drops any path ending in /health or
	// /health-check (probes outnumber real traffic).
	Skip func(c fiber.Ctx) bool
	// Headers are request headers to include under "headers". Sensitive ones
	// (Authorization, Cookie, ...) are logged as RedactedValue.
	Headers []string
}

type accessLine struct {
	Msg       string            `json:"msg"`
	Time      string            `json:"time"`
	Status    int               `json:"status"`
	Latency   string            `json:"latency"`
	Method    string            `json:"method"`
	Path      string            `json:"path"`
	RequestID string            `json:"request_id"`
	Headers   map[string]string `json:"headers,omitempty"`
}

// AccessLog writes one JSON object per request. Unlike Fiber's logger tag
// format it fixes two defects ctech-billing found in wallet, poker and dfe:
// `${request-id}` is not a Fiber tag so request_id was always empty (here it is
// read from the context set by RequestID, falling back to the response header,
// so mounting order does not matter), and ANSI colour codes leaked into the JSON
// when the stream was stdout (here nothing is ever coloured). It also logs the
// real status of a failed request by running the app's ErrorHandler first
// instead of reporting the 200 that has not been overwritten yet. The query
// string is never logged (path only).
func AccessLog(config ...AccessLogConfig) fiber.Handler {
	var cfg AccessLogConfig
	if len(config) > 0 {
		cfg = config[0]
	}
	out := cfg.Output
	if out == nil {
		out = os.Stdout
	}
	skip := cfg.Skip
	if skip == nil {
		skip = skipHealth
	}
	var mu sync.Mutex // serialises writers that are not themselves atomic

	return func(c fiber.Ctx) error {
		if skip(c) {
			return c.Next()
		}
		start := time.Now()
		err := c.Next()
		if err != nil {
			if handlerErr := c.App().ErrorHandler(c, err); handlerErr != nil {
				_ = c.SendStatus(statusFromError(handlerErr))
			}
		}
		line := accessLine{
			Msg:       AccessLogMessage,
			Time:      start.UTC().Format(time.RFC3339Nano),
			Status:    c.Response().StatusCode(),
			Latency:   time.Since(start).String(),
			Method:    c.Method(),
			Path:      c.Path(),
			RequestID: requestIDOf(c),
			Headers:   logHeaders(c, cfg.Headers),
		}
		if b, mErr := json.Marshal(line); mErr == nil {
			mu.Lock()
			_, _ = out.Write(append(b, '\n'))
			mu.Unlock()
		}
		return nil
	}
}

func skipHealth(c fiber.Ctx) bool {
	p := c.Path()
	return strings.HasSuffix(p, healthSuffix) || strings.HasSuffix(p, healthCheckSuffix)
}

func requestIDOf(c fiber.Ctx) string {
	if id := observability.RequestID(c.Context()); id != "" {
		return id
	}
	return c.GetRespHeader(RequestIDHeader)
}

func logHeaders(c fiber.Ctx, names []string) map[string]string {
	if len(names) == 0 {
		return nil
	}
	m := make(map[string]string, len(names))
	for _, n := range names {
		v := c.Get(n)
		if v == "" {
			continue
		}
		if _, bad := sensitiveHeaders[strings.ToLower(n)]; bad {
			v = RedactedValue
		}
		m[http.CanonicalHeaderKey(n)] = v
	}
	return m
}

func statusFromError(err error) int {
	var fe *fiber.Error
	if errors.As(err, &fe) {
		return fe.Code
	}
	return fiber.StatusInternalServerError
}
