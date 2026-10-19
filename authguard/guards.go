package authguard

import (
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"gopkg.aoctech.app/api-commons/problem"
)

// DefaultStepUpMaxAge is the MFA freshness window used when RequireRecentMFA
// gets a non-positive maxAge (account and wallet both use 5 minutes).
const DefaultStepUpMaxAge = 5 * time.Minute

// bearerInsufficientScope is the RFC 6750 challenge prefix; the scope list and a closing quote follow.
const bearerInsufficientScope = `Bearer error="insufficient_scope", scope="`

// now is replaced by tests.
var now = time.Now

func forbidScope(c fiber.Ctx, scopes []string) error {
	c.Set(fiber.HeaderWWWAuthenticate, bearerInsufficientScope+strings.Join(scopes, " ")+`"`)
	return send(c, problem.Forbidden(detailInsufficientScope))
}

// RequireScope passes callers whose token carries scope. Register after the auth
// middleware. 401 without claims, 403 with a WWW-Authenticate insufficient_scope
// challenge otherwise.
func RequireScope(scope string) fiber.Handler { return RequireAnyScope(scope) }

// RequireAnyScope passes callers holding at least one of scopes. With no scopes
// it denies everything: an empty policy must not mean "open".
func RequireAnyScope(scopes ...string) fiber.Handler {
	return func(c fiber.Ctx) error {
		cl := GetClaims(c)
		if cl == nil {
			return unauthenticated(c)
		}
		for _, s := range scopes {
			if cl.HasScope(s) {
				return c.Next()
			}
		}
		return forbidScope(c, scopes)
	}
}

// RequireAllScopes passes callers holding every one of scopes. With no scopes
// it denies everything, like RequireAnyScope.
func RequireAllScopes(scopes ...string) fiber.Handler {
	return func(c fiber.Ctx) error {
		cl := GetClaims(c)
		if cl == nil {
			return unauthenticated(c)
		}
		if len(scopes) == 0 {
			return forbidScope(c, scopes)
		}
		for _, s := range scopes {
			if !cl.HasScope(s) {
				return forbidScope(c, scopes)
			}
		}
		return c.Next()
	}
}

// RequireInternalScope gates a service-to-service route: only a
// client_credentials token (empty sid) carrying scope passes. A user or API-key
// token has a sid and is refused even if it holds the scope, because the
// user/M2M boundary is the point, not the scope string.
func RequireInternalScope(scope string) fiber.Handler {
	return func(c fiber.Ctx) error {
		cl := GetClaims(c)
		if cl == nil {
			return unauthenticated(c)
		}
		if cl.SID != "" {
			return send(c, problem.Forbidden(detailServiceTokenOnly))
		}
		if !cl.HasScope(scope) {
			return forbidScope(c, []string{scope})
		}
		return c.Next()
	}
}

// RequireRecentMFA demands an MFA proof (last_mfa_at claim) newer than maxAge,
// else 403 step-up-required carrying max_age_seconds. Stateless: after a
// successful challenge the client silent-refreshes to get a new claim. A
// last_mfa_at in the future (clock skew) counts as fresh. maxAge <= 0 means
// DefaultStepUpMaxAge.
func RequireRecentMFA(maxAge time.Duration) fiber.Handler {
	if maxAge <= 0 {
		maxAge = DefaultStepUpMaxAge
	}
	return func(c fiber.Ctx) error {
		cl := GetClaims(c)
		if cl == nil {
			return unauthenticated(c)
		}
		if cl.LastMFAAt == 0 || now().Sub(time.Unix(cl.LastMFAAt, 0)) > maxAge {
			return stepUpRequired(c, int(maxAge.Seconds()))
		}
		return c.Next()
	}
}

// RequireClientID passes only tokens whose azp (OAuth client_id) is one of ids,
// for protocol helpers meant for a service's own frontend. With no ids it
// denies everything.
func RequireClientID(ids ...string) fiber.Handler {
	return func(c fiber.Ctx) error {
		cl := GetClaims(c)
		if cl == nil {
			return unauthenticated(c)
		}
		for _, id := range ids {
			if id != "" && cl.AZP == id {
				return c.Next()
			}
		}
		return send(c, problem.Forbidden(detailWrongClient))
	}
}
