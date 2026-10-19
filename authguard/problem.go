package authguard

import (
	"encoding/json"
	"net/http"

	"github.com/gofiber/fiber/v3"

	"gopkg.aoctech.app/api-commons/problem"
)

// Problem types owned by this package, relative like the ones in package problem.
const (
	TypeStepUpRequired     = "/problems/step-up-required"
	TypeServiceUnavailable = "/problems/service-unavailable"
)

const (
	contentTypeProblem      = "application/problem+json"
	detailMissingCreds      = "missing credentials"
	detailInsufficientScope = "insufficient scope"
	detailServiceTokenOnly  = "this route accepts service tokens only"
	detailWrongClient       = "this endpoint is not available to this client"
	detailStepUp            = "this operation requires recent multi-factor authentication"
	detailSessionEnded      = "session ended"
	detailRevocationDown    = "could not validate the session; try again"
	titleStepUp             = "Step-up Authentication Required"
	titleUnavailable        = "Service Unavailable"
)

// send writes p as application/problem+json. The common problem package defines
// the body, not the Fiber writer, so the guards share this one.
func send(c fiber.Ctx, p *problem.Problem) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	c.Status(p.Status)
	c.Set(fiber.HeaderContentType, contentTypeProblem)
	return c.Send(b)
}

func unauthenticated(c fiber.Ctx) error {
	return send(c, problem.Unauthorized(detailMissingCreds).WithNextAction(problem.NextActionReauthenticate, 0))
}

func stepUpRequired(c fiber.Ctx, maxAgeSeconds int) error {
	p := problem.New(http.StatusForbidden, TypeStepUpRequired, titleStepUp, detailStepUp)
	p.MaxAgeSeconds = maxAgeSeconds
	return send(c, p)
}

func serviceUnavailable(c fiber.Ctx, cause error) error {
	p := problem.New(http.StatusServiceUnavailable, TypeServiceUnavailable, titleUnavailable, detailRevocationDown).
		WithNextAction(problem.NextActionRetry, 0).WithCause(cause)
	return send(c, p)
}
