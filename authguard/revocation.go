package authguard

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/gofiber/fiber/v3"

	"gopkg.aoctech.app/api-commons/cache"
	"gopkg.aoctech.app/api-commons/jwtverify"
	"gopkg.aoctech.app/api-commons/problem"
)

// Policy says what a route does when the revocation list cannot be read.
type Policy int

const (
	// PolicyReadOpen: an unreachable list lets the request through (logged).
	// For reads: an outage must not take the product down, and the exposure is
	// bounded by the token lifetime. A revoked entry that IS readable still gives 401.
	PolicyReadOpen Policy = iota
	// PolicyWriteClosed: an unreachable list is a 503, never a pass. For writes,
	// money and consent, where honouring a revoked session is worse than an outage.
	PolicyWriteClosed
)

// PolicyForMethod returns PolicyReadOpen for safe methods (GET, HEAD, OPTIONS)
// and PolicyWriteClosed for everything else, so an unknown verb fails closed.
func PolicyForMethod(method string) Policy {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return PolicyReadOpen
	}
	return PolicyWriteClosed
}

// RequireUnrevoked re-checks ctech-account's revocation list (jwtverify.CheckRevoked,
// always a live read) under policy. A nil backend disables it: for route tests
// only, since production config must provide the revocation backend.
// Register after the auth middleware.
func RequireUnrevoked(revocation cache.Backend, policy Policy) fiber.Handler {
	return func(c fiber.Ctx) error { return checkRevocation(c, revocation, policy) }
}

// RequireUnrevokedByMethod is RequireUnrevoked with the policy chosen per
// request from the HTTP method (PolicyForMethod): one middleware for a whole
// router group, reads open and writes closed.
func RequireUnrevokedByMethod(revocation cache.Backend) fiber.Handler {
	return func(c fiber.Ctx) error { return checkRevocation(c, revocation, PolicyForMethod(c.Method())) }
}

func checkRevocation(c fiber.Ctx, revocation cache.Backend, policy Policy) error {
	if revocation == nil {
		return c.Next()
	}
	cl := GetClaims(c)
	if cl == nil {
		return unauthenticated(c)
	}
	err := jwtverify.CheckRevoked(c.Context(), revocation, cl.Sub, cl.IssuedAt)
	switch {
	case err == nil:
		return c.Next()
	case errors.Is(err, jwtverify.ErrTokenRevoked):
		return send(c, problem.Unauthorized(detailSessionEnded).WithNextAction(problem.NextActionReauthenticate, 0))
	case policy == PolicyReadOpen:
		slog.WarnContext(c.Context(), "authguard: revocation check skipped (read-open)", "error", err)
		return c.Next()
	default:
		return serviceUnavailable(c, err)
	}
}
