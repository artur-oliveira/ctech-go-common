// Package authguard holds the Fiber v3 authorization middlewares every CTech API
// stacks after its token-verification middleware: scope checks, internal
// (client_credentials) routes, recent-MFA step-up, OAuth client pinning and the
// revocation policy. Token parsing stays in jwtverify; this package only reads
// the *jwtverify.Claims the auth middleware stored with SetClaims.
package authguard

import (
	"github.com/gofiber/fiber/v3"

	"gopkg.aoctech.app/api-commons/jwtverify"
)

// LocalsClaims is the Fiber locals key holding the caller's *jwtverify.Claims.
// "claims" matches the key ctech-wallet already uses.
const LocalsClaims = "claims"

// SetClaims stores the verified claims for the guards below. The auth
// middleware of each service calls it once after jwtverify succeeds.
func SetClaims(c fiber.Ctx, cl *jwtverify.Claims) { c.Locals(LocalsClaims, cl) }

// GetClaims is the single accessor for the caller's claims; nil means the
// request was not authenticated (the guards answer 401 in that case).
func GetClaims(c fiber.Ctx) *jwtverify.Claims {
	cl, _ := c.Locals(LocalsClaims).(*jwtverify.Claims)
	return cl
}
