// Package fiberlimit is the Fiber middleware over ratelimit.Bucket. It is a
// separate package so ratelimit itself stays free of any HTTP framework.
package fiberlimit

import (
	"math"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"

	"gopkg.aoctech.app/api-commons/ratelimit"
)

// New returns a middleware that takes a token from b for key(c) and, when none is
// left, sets Retry-After and answers deny(c, seconds), the service's own 429 (each
// API renders its problems its own way; problem.TooManyRequestsAfter is the shared
// body). An empty key (no identity resolved) is not limited. A nil bucket is a
// pass-through, so a limit set to zero costs nothing.
func New(b *ratelimit.Bucket, key func(fiber.Ctx) string, deny func(c fiber.Ctx, retryAfterSeconds int) error) fiber.Handler {
	if b == nil {
		return func(c fiber.Ctx) error { return c.Next() }
	}
	return func(c fiber.Ctx) error {
		k := key(c)
		if k == "" {
			return c.Next()
		}
		ok, wait := b.Allow(k)
		if ok {
			return c.Next()
		}
		secs := int(math.Ceil(wait.Round(time.Millisecond).Seconds()))
		secs = max(secs, 1)
		c.Set(fiber.HeaderRetryAfter, strconv.Itoa(secs))
		return deny(c, secs)
	}
}
