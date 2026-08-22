// Package fiber integrates the shared observability contract with Fiber v3.
package fiber

import (
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"

	"gopkg.aoctech.app/api-commons/observability"
)

// RequestIDHeader is the canonical platform correlation header.
const RequestIDHeader = fiber.HeaderXRequestID

// RequestIDConfig customizes correlation while preserving the platform
// defaults. LocalsKey is optional and exists for services whose audit records
// also read the request ID from Fiber locals.
type RequestIDConfig struct {
	Header    string
	LocalsKey any
	Generator func() string
}

// RequestID assigns or preserves a validated correlation ID, echoes it in the
// response, and attaches it to context.Context for downstream logging.
func RequestID(config ...RequestIDConfig) fiber.Handler {
	cfg := RequestIDConfig{Header: RequestIDHeader, Generator: requestid.ConfigDefault.Generator}
	if len(config) > 0 {
		if config[0].Header != "" {
			cfg.Header = config[0].Header
		}
		if config[0].Generator != nil {
			cfg.Generator = config[0].Generator
		}
		cfg.LocalsKey = config[0].LocalsKey
	}

	return func(c fiber.Ctx) error {
		requestID := c.Get(cfg.Header)
		if !validRequestID(requestID) {
			requestID = generatedRequestID(cfg.Generator)
		}
		c.Set(cfg.Header, requestID)
		if cfg.LocalsKey != nil {
			c.Locals(cfg.LocalsKey, requestID)
		}
		c.SetContext(observability.WithRequestID(c.Context(), requestID))
		return c.Next()
	}
}

func generatedRequestID(generator func() string) string {
	for range 3 {
		if requestID := generator(); validRequestID(requestID) {
			return requestID
		}
	}
	return requestid.ConfigDefault.Generator()
}

// LogHTTPError records an HTTP rejection/failure with method, path, problem
// type, internal cause, and request ID when middleware correlation is active.
func LogHTTPError(c fiber.Ctx, status int, problemType string, err error, attrs ...any) {
	observability.LogHTTPError(c.Context(), status, c.Method(), c.Path(), problemType, err, attrs...)
}

// RequestIDFromContext returns the current request correlation identifier.
func RequestIDFromContext(c fiber.Ctx) string {
	return observability.RequestID(c.Context())
}

func validRequestID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for i := range len(value) {
		if value[i] < 0x21 || value[i] > 0x7e {
			return false
		}
	}
	return true
}
