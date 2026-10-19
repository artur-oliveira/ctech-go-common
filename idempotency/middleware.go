package idempotency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"

	"gopkg.aoctech.app/api-commons/problem"
)

// Wire contract.
const (
	HeaderKey    = "Idempotency-Key"
	HeaderReplay = "Idempotent-Replay"
	// MaxKeyLength bounds a key: long enough for a UUID or a composite id,
	// short enough that it is not a place to store data.
	MaxKeyLength = 128
	// DefaultMaxBodyBytes caps the answer that is stored (DynamoDB items are
	// limited to 400 KB). A larger answer is served but not recorded.
	DefaultMaxBodyBytes = 256 * 1024
	// DefaultStoreTimeout bounds each store call.
	DefaultStoreTimeout = 2 * time.Second
	// inProgressRetryAfterSeconds is advertised on an in-progress refusal.
	inProgressRetryAfterSeconds = 1
	replayTrue                  = "true"
	mimeProblem                 = "application/problem+json"
)

// Problem types (RFC 7807).
const (
	TypeConflict        = "/problems/idempotency-conflict"
	TypeInProgress      = "/problems/idempotency-in-progress"
	TypeKeyRequired     = "/problems/idempotency-key-required"
	TypeKeyInvalid      = "/problems/idempotency-key-invalid"
	TypeUnavailable     = "/problems/idempotency-unavailable"
	TypeScopeUnresolved = "/problems/idempotency-scope-unresolved"
)

// ScopeFunc names the namespace a key belongs to: a credential, tenant or user
// id (include the mode, e.g. livemode, if the same id spans modes). ok=false
// means it could not be resolved; the request is then refused with 500, since
// guessing a scope would let two tenants share keys.
type ScopeFunc func(c fiber.Ctx) (scope string, ok bool)

// ScopeFromLocals reads the scope from a string stored with c.Locals by an
// earlier (auth) middleware.
func ScopeFromLocals(localsKey string) ScopeFunc {
	return func(c fiber.Ctx) (string, bool) {
		s, ok := c.Locals(localsKey).(string)
		return s, ok && s != ""
	}
}

// Config configures New. Store and Scope are required.
type Config struct {
	Store Store
	Scope ScopeFunc

	// Required makes a missing Idempotency-Key a 400. When false, a request
	// without the header runs normally, unprotected. Build one handler per
	// policy to mix them across routes.
	Required bool
	// Methods the middleware applies to; others pass through untouched.
	// Default: POST, PUT, PATCH, DELETE.
	Methods []string
	// TTL a recorded answer is replayable for. Default DefaultTTL; must not be
	// negative.
	TTL time.Duration
	// Lease a claim holds without an answer. Default DefaultLease.
	Lease time.Duration
	// StoreTimeout bounds each store call. Default DefaultStoreTimeout.
	StoreTimeout time.Duration
	// MaxBodyBytes caps the stored answer. Default DefaultMaxBodyBytes.
	MaxBodyBytes int
	// FailOpen decides what happens when the store is unreachable. false (the
	// default, for money and other non-repeatable effects): refuse with 503 and
	// Retry-After. true: run the handler unprotected and log a warning.
	FailOpen bool
}

func (c *Config) defaults() {
	if len(c.Methods) == 0 {
		c.Methods = []string{fiber.MethodPost, fiber.MethodPut, fiber.MethodPatch, fiber.MethodDelete}
	}
	if c.TTL == 0 {
		c.TTL = DefaultTTL
	}
	if c.Lease == 0 {
		c.Lease = DefaultLease
	}
	if c.StoreTimeout == 0 {
		c.StoreTimeout = DefaultStoreTimeout
	}
	if c.MaxBodyBytes == 0 {
		c.MaxBodyBytes = DefaultMaxBodyBytes
	}
}

// New returns the middleware. It panics on a nil Store or Scope or a negative
// duration: a wiring mistake that must fail at startup, not on a request.
//
// Order: after authentication (Scope needs the identity) and after the
// deadline middleware.
//
// A request with a key is claimed with one conditional write before the
// handler runs, so concurrent requests with one key run the handler once. The
// answer is recorded only when it is final: 2xx, or a 4xx that does not depend
// on who or when (see recordable). A 5xx, a handler error, a non-final 4xx or
// an oversized answer gives the claim back so the retry runs.
func New(cfg Config) fiber.Handler {
	if cfg.Store == nil || cfg.Scope == nil {
		panic("idempotency: Store and Scope are required")
	}
	if cfg.TTL < 0 || cfg.Lease < 0 || cfg.StoreTimeout < 0 || cfg.MaxBodyBytes < 0 {
		panic("idempotency: negative Config value")
	}
	cfg.defaults()

	return func(c fiber.Ctx) error {
		if !slices.Contains(cfg.Methods, c.Method()) {
			return c.Next()
		}
		key := c.Get(HeaderKey)
		if key == "" {
			if cfg.Required {
				return send(c, problem.New(http.StatusBadRequest, TypeKeyRequired, "Idempotency Key Required",
					HeaderKey+" header is required"))
			}
			return c.Next()
		}
		if !validKey(key) {
			return send(c, problem.New(http.StatusBadRequest, TypeKeyInvalid, "Idempotency Key Invalid",
				HeaderKey+" must be 1-"+strconv.Itoa(MaxKeyLength)+" visible ASCII characters"))
		}
		scope, ok := cfg.Scope(c)
		if !ok {
			return send(c, problem.New(http.StatusInternalServerError, TypeScopeUnresolved, "Idempotency Scope Unresolved", "idempotency scope not resolved"))
		}

		hash := hashRequest(c)
		ctx, cancel := context.WithTimeout(c.Context(), cfg.StoreTimeout)
		existing, err := cfg.Store.Claim(ctx, scope, key, hash, cfg.Lease)
		cancel()
		switch {
		case errors.Is(err, ErrConflict):
			return send(c, problem.New(http.StatusConflict, TypeConflict, "Idempotency Conflict",
				"this "+HeaderKey+" was already used with a different request"))
		case errors.Is(err, ErrInProgress):
			c.Set(fiber.HeaderRetryAfter, strconv.Itoa(inProgressRetryAfterSeconds))
			return send(c, problem.New(http.StatusConflict, TypeInProgress, "Request In Progress",
				"a request with this "+HeaderKey+" is still running; retry to get its answer").
				WithNextAction(problem.NextActionRetry, inProgressRetryAfterSeconds))
		case err != nil:
			slog.ErrorContext(c.Context(), "idempotency claim failed", "fail_open", cfg.FailOpen, "reason", err.Error())
			if cfg.FailOpen {
				return c.Next()
			}
			c.Set(fiber.HeaderRetryAfter, strconv.Itoa(inProgressRetryAfterSeconds))
			return send(c, problem.New(http.StatusServiceUnavailable, TypeUnavailable, "Idempotency Unavailable",
				"could not guarantee idempotency; retry").
				WithNextAction(problem.NextActionRetry, inProgressRetryAfterSeconds).WithCause(err))
		case existing != nil:
			// Replayed verbatim (status and body), so a retry is indistinguishable
			// from the original apart from the replay header.
			if existing.ContentType != "" {
				c.Set(fiber.HeaderContentType, existing.ContentType)
			}
			c.Set(HeaderReplay, replayTrue)
			return c.Status(existing.Status).Send(existing.Body)
		}

		herr := c.Next()
		status := c.Response().StatusCode()
		body := c.Response().Body()

		// The request context may be done (client gone); the operation already
		// ran, so the bookkeeping must still be attempted, with its own bound.
		sctx, scancel := context.WithTimeout(context.WithoutCancel(c.Context()), cfg.StoreTimeout)
		defer scancel()
		if herr != nil || !recordable(status) || len(body) > cfg.MaxBodyBytes {
			if rerr := cfg.Store.Release(sctx, scope, key); rerr != nil {
				slog.WarnContext(sctx, "idempotency claim not released; it lapses with its lease", "reason", rerr.Error())
			}
			return herr
		}
		rec := Record{
			RequestHash: hash,
			Status:      status,
			ContentType: string(c.Response().Header.ContentType()),
			Body:        slices.Clone(body),
		}
		if err := cfg.Store.Complete(sctx, scope, key, rec, cfg.TTL); err != nil {
			// The operation happened; failing the response would tell the caller it
			// did not. The claim stays until its lease ends, so a retry meanwhile is
			// refused (in progress), not re-run.
			slog.WarnContext(sctx, "idempotency answer not recorded", "reason", err.Error())
		}
		return nil
	}
}

// recordable: the answer is final and safe to replay. 2xx and 4xx that depend
// only on the request. Not recorded: 5xx and 1xx/3xx (transient or not an
// answer), and 4xx that depend on who or when (credentials, locks, load,
// ordering) where a fixed retry with the same key must run.
func recordable(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestTimeout,
		http.StatusConflict, http.StatusLocked, http.StatusTooEarly, http.StatusTooManyRequests:
		return false
	}
	return (status >= 200 && status < 300) || (status >= 400 && status < 500)
}

func validKey(k string) bool {
	if len(k) == 0 || len(k) > MaxKeyLength {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x21 || k[i] > 0x7e {
			return false
		}
	}
	return true
}

// hashRequest fingerprints method, concrete path (route params included, so
// /wallets/1/debit and /wallets/2/debit differ), query and body, NUL-separated.
func hashRequest(c fiber.Ctx) string {
	h := sha256.New()
	for _, part := range [][]byte{
		[]byte(c.Method()), []byte(c.Path()), c.Request().URI().QueryString(), c.Body(),
	} {
		h.Write(part)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// send renders p as application/problem+json itself, so the middleware does
// not depend on the service's error handler.
func send(c fiber.Ctx, p *problem.Problem) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	c.Set(fiber.HeaderContentType, mimeProblem)
	return c.Status(p.Status).Send(b)
}
