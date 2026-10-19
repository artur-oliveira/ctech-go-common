package fiberlimit

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/gofiber/fiber/v3"

	"gopkg.aoctech.app/api-commons/problem"
	"gopkg.aoctech.app/api-commons/ratelimit"
)

// Response headers (IETF draft-ietf-httpapi-ratelimit-headers) and media type.
const (
	HeaderRateLimitLimit     = "RateLimit-Limit"
	HeaderRateLimitRemaining = "RateLimit-Remaining"
	HeaderRateLimitReset     = "RateLimit-Reset"
	contentTypeProblem       = "application/problem+json"

	// TypeServiceUnavailable is the problem type of the fail-closed 503.
	TypeServiceUnavailable = "/problems/service-unavailable"

	detailTooManyRequests = "Too many requests. Please slow down and try again later."
	detailUnavailable     = "Rate limiting is temporarily unavailable. Please try again later."
)

// Count says which responses spend the limiter's budget.
type Count int

const (
	// CountAll spends one unit per request (throughput cap). Atomic.
	CountAll Count = iota
	// CountFailures spends one unit per 4xx/5xx response (brute-force guard):
	// the budget is read before the handler and spent after it. A handler can
	// exempt a response with SkipCount.
	CountFailures
	// CountSuccesses spends one unit per successful response (budget for an
	// action that typos and rejected attempts must not consume).
	CountSuccesses
)

const localsSkipCount = "ratelimit_skip_count"

// SkipCount exempts the current response from CountFailures. Use it only for
// failures that need a real (if stale) credential, never for guessable ones.
func SkipCount(c fiber.Ctx) { c.Locals(localsSkipCount, true) }

// PolicyConfig wires a ratelimit.Limiter (limit, window, FailMode) to Fiber.
type PolicyConfig struct {
	Limiter *ratelimit.Limiter
	// Key resolves the caller identity (see ratelimit.Key, IPKey). "" is not limited.
	Key   func(fiber.Ctx) string
	Count Count
	// Deny and Unavailable render the 429 and the fail-closed 503. Nil means a
	// RFC 7807 problem (problem.TooManyRequestsAfter / 503); set them to keep a
	// service's own problem type. Retry-After and RateLimit-* are set before
	// they are called.
	Deny        func(c fiber.Ctx, retryAfterSeconds int) error
	Unavailable func(c fiber.Ctx) error
}

// NewPolicy returns the middleware for cfg. A nil Limiter or Key is a
// pass-through (limiter switched off).
func NewPolicy(cfg PolicyConfig) fiber.Handler {
	if cfg.Limiter == nil || cfg.Key == nil {
		return func(c fiber.Ctx) error { return c.Next() }
	}
	deny, unavailable := cfg.Deny, cfg.Unavailable
	if deny == nil {
		deny = func(c fiber.Ctx, secs int) error {
			return sendProblem(c, problem.TooManyRequestsAfter(detailTooManyRequests, secs))
		}
	}
	if unavailable == nil {
		unavailable = func(c fiber.Ctx) error {
			return sendProblem(c, problem.New(http.StatusServiceUnavailable, TypeServiceUnavailable,
				"Service Unavailable", detailUnavailable).WithNextAction(problem.NextActionRetry, 0))
		}
	}
	lim := cfg.Limiter
	return func(c fiber.Ctx) error {
		id := cfg.Key(c)
		if id == "" {
			return c.Next()
		}
		var d ratelimit.Decision
		var err error
		if cfg.Count == CountAll {
			d, err = lim.Decide(c.Context(), id)
		} else {
			d, err = lim.DecideFailures(c.Context(), id)
		}
		if err != nil {
			slog.WarnContext(c.Context(), "rate limiter backend error", "prefix", lim.Prefix, "mode", failMode(lim), "err", err)
		}
		switch d.Outcome {
		case ratelimit.Unavailable:
			return unavailable(c)
		case ratelimit.Deny:
			setHeaders(c, d)
			c.Set(fiber.HeaderRetryAfter, strconv.Itoa(d.RetryAfterSeconds()))
			return deny(c, d.RetryAfterSeconds())
		}
		if err == nil {
			setHeaders(c, d)
		}
		if cfg.Count == CountAll {
			return c.Next()
		}
		nextErr := c.Next()
		skip, _ := c.Locals(localsSkipCount).(bool)
		failed := nextErr != nil || c.Response().StatusCode() >= fiber.StatusBadRequest
		if (cfg.Count == CountFailures && failed && !skip) || (cfg.Count == CountSuccesses && !failed) {
			// A failed record never blocks the already-produced response.
			if rerr := lim.RecordFailure(c.Context(), id); rerr != nil {
				slog.ErrorContext(c.Context(), "rate limiter failed to count", "prefix", lim.Prefix, "err", rerr)
			}
		}
		return nextErr
	}
}

func failMode(l *ratelimit.Limiter) string {
	if l.FailClosed {
		return ratelimit.FailClosed.String()
	}
	return l.FailMode.String()
}

func setHeaders(c fiber.Ctx, d ratelimit.Decision) {
	c.Set(HeaderRateLimitLimit, strconv.FormatInt(d.Limit, 10))
	c.Set(HeaderRateLimitRemaining, strconv.FormatInt(d.Remaining, 10))
	c.Set(HeaderRateLimitReset, strconv.Itoa(d.RetryAfterSeconds()))
}

func sendProblem(c fiber.Ctx, p *problem.Problem) error {
	return c.Status(p.Status).JSON(p, contentTypeProblem)
}

// IPKey is a Key func: the client IP resolved by ex (nil ex means the TCP peer;
// see ratelimit.IPExtractor for the X-Forwarded-For rules). Combine with other
// parts through ratelimit.Key for composite identities.
func IPKey(ex *ratelimit.IPExtractor) func(fiber.Ctx) string {
	return func(c fiber.Ctx) string {
		var xff []string
		for _, v := range c.Request().Header.PeekAll(ratelimit.ForwardedForHeader) {
			xff = append(xff, string(v))
		}
		return ex.ClientIP(c.RequestCtx().RemoteAddr().String(), xff...)
	}
}
