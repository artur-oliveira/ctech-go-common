package pushsvc

import (
	"regexp"

	"github.com/gofiber/fiber/v3"

	"gopkg.aoctech.app/api-commons/problem"
	"gopkg.aoctech.app/api-commons/validation"
)

// deviceID is the id a path accepts: SubscriptionID, 16 hex characters.
var deviceID = regexp.MustCompile(`^[0-9a-f]{16}$`)

type subscriptionBody struct {
	Endpoint string `json:"endpoint" validate:"required,max=2048"`
	Keys     struct {
		P256dh string `json:"p256dh" validate:"required,max=128"`
		Auth   string `json:"auth" validate:"required,max=64"`
	} `json:"keys" validate:"required"`
}

// Mount registers GET /push/config and PUT|GET /push/subscriptions, DELETE /push/subscriptions/:id under router. auth
// authenticates, userID reads the caller's id from the request, and extra (scope checks, say) run after auth on every
// route. POST /push/test is the product's: it needs the product's job invoker.
func Mount(router fiber.Router, svc *Service, auth fiber.Handler, userID func(fiber.Ctx) string, extra ...fiber.Handler) {
	handlers := []any{auth}
	for _, h := range extra {
		handlers = append(handlers, h)
	}
	g := router.Group("/push", handlers...)
	send := func(c fiber.Ctx, err error) error { return problem.FromError(err).Send(c) }

	g.Get("/config", func(c fiber.Ctx) error { return c.JSON(svc.Config()) })

	g.Put("/subscriptions", func(c fiber.Ctx) error {
		var body subscriptionBody
		if p := validation.BindJSON(c.Body(), &body); p != nil {
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
