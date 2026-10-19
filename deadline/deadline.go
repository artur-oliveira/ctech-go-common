// Package deadline gives every request a time budget. Fiber v3's c.Context() is
// context.Background() unless a middleware sets another one, so without this a
// DynamoDB, Valkey or HTTP call made for a request runs to its own timeout (or
// forever) after the client gave up, holding a goroutine and a connection.
//
// It bounds the work a handler does through that context (AWS SDK, Valkey,
// outbound HTTP all honour it). It does not interrupt code that ignores the
// context, and a client disconnect is not seen by fasthttp, so the deadline is
// the only way the work ends early.
package deadline

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
)

// New returns a middleware that derives the request context with timeout and
// turns a deadline error that reaches it into 504. A handler that renders its own
// errors keeps its own mapping (map context.DeadlineExceeded to 504 there). A WebSocket upgrade is never bounded (the
// connection outlives any request budget); skip names any other route that does,
// such as an export or a server-sent-events stream.
func New(timeout time.Duration, skip ...func(fiber.Ctx) bool) fiber.Handler {
	return func(c fiber.Ctx) error {
		if strings.EqualFold(c.Get(fiber.HeaderUpgrade), "websocket") {
			return c.Next()
		}
		for _, s := range skip {
			if s(c) {
				return c.Next()
			}
		}
		ctx, cancel := context.WithTimeout(c.Context(), timeout)
		defer cancel()
		c.SetContext(ctx)

		err := c.Next()
		if err != nil && errors.Is(err, context.DeadlineExceeded) {
			return fiber.NewError(fiber.StatusGatewayTimeout, "the request took too long")
		}
		return err
	}
}
