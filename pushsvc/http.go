package pushsvc

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"

	"github.com/gofiber/fiber/v3"

	"gopkg.aoctech.app/api-commons/problem"
)

// deviceID is the id a path accepts: SubscriptionID, 16 hex characters.
var deviceID = regexp.MustCompile(`^[0-9a-f]{16}$`)

// Bounds of a browser's PushSubscription.toJSON(): the endpoint, the P-256 point and the 16-byte secret in base64url.
const (
	maxBodyBytes    = 16 << 10
	maxEndpointLen  = 2048
	maxP256dhLen    = 128
	maxAuthLen      = 64
	fieldTooLongMsg = "valor longo demais"
)

type subscriptionBody struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// bindSubscription reads the body strictly (no unknown field, nothing after the object, at most maxBodyBytes) and checks
// each field is present and bounded. It does not use go-common's validation package on purpose: that pulls the validator,
// mimetype and x/text into every binary of a product that imports pushsvc, about 1.5 MB each.
func bindSubscription(c fiber.Ctx, dst *subscriptionBody) *problem.Problem {
	raw := c.Body()
	if len(raw) > maxBodyBytes {
		return problem.New(fiber.StatusRequestEntityTooLarge, problem.TypeBadRequest, "Payload Too Large", "request body too large")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return problem.BadRequest("corpo da requisição inválido")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return problem.BadRequest("corpo da requisição inválido")
	}
	var errs []problem.FieldError
	check := func(field, value string, max int) {
		switch {
		case value == "":
			errs = append(errs, problem.Field(field, "required", "campo obrigatório"))
		case len(value) > max:
			errs = append(errs, problem.Field(field, "max", fieldTooLongMsg))
		}
	}
	check("endpoint", dst.Endpoint, maxEndpointLen)
	check("keys.p256dh", dst.Keys.P256dh, maxP256dhLen)
	check("keys.auth", dst.Keys.Auth, maxAuthLen)
	if len(errs) > 0 {
		return problem.Validation(errs)
	}
	return nil
}

// Mount registers GET /push/config and PUT|GET /push/subscriptions, DELETE /push/subscriptions/:id under router. auth
// authenticates, userID reads the caller's id from the request, and extra (scope checks, say) run after auth on every
// route. POST /push/test is the product's: it needs the product's job invoker.
func Mount(router fiber.Router, svc *Service, auth fiber.Handler, userID func(fiber.Ctx) string, extra ...fiber.Handler) {
	handlers := []any{auth}
	for _, h := range extra {
		handlers = append(handlers, h)
	}
	// An empty id would address the shared partition USER#: refuse it before any route runs.
	handlers = append(handlers, func(c fiber.Ctx) error {
		if userID(c) == "" {
			return problem.Unauthorized("credenciais ausentes").Send(c)
		}
		return c.Next()
	})
	g := router.Group("/push", handlers...)
	send := func(c fiber.Ctx, err error) error { return problem.FromError(err).Send(c) }

	g.Get("/config", func(c fiber.Ctx) error { return c.JSON(svc.Config()) })

	g.Put("/subscriptions", func(c fiber.Ctx) error {
		var body subscriptionBody
		if p := bindSubscription(c, &body); p != nil {
			return p.Send(c)
		}
		dev, err := svc.Subscribe(c.Context(), userID(c), SubscriptionInput{
			Endpoint: body.Endpoint, P256dh: body.Keys.P256dh, Auth: body.Keys.Auth, UserAgent: c.Get(fiber.HeaderUserAgent),
		})
		if err != nil {
			return send(c, err)
		}
		return c.JSON(dev)
	})

	g.Get("/subscriptions", func(c fiber.Ctx) error {
		devices, err := svc.Devices(c.Context(), userID(c))
		if err != nil {
			return send(c, err)
		}
		return c.JSON(fiber.Map{"data": devices, "max_subscriptions": MaxSubscriptionsPerUser})
	})

	g.Delete("/subscriptions/:id", func(c fiber.Ctx) error {
		id := c.Params("id")
		if !deviceID.MatchString(id) {
			return problem.BadRequest("identificador de dispositivo inválido").Send(c)
		}
		if err := svc.Unsubscribe(c.Context(), userID(c), id); err != nil {
			return send(c, err)
		}
		return c.SendStatus(fiber.StatusNoContent)
	})
}
