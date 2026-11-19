package pushsvc

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
)

func newApp(t *testing.T, svc *Service) *fiber.App {
	t.Helper()
	app := fiber.New()
	auth := func(c fiber.Ctx) error { c.Locals("uid", "u1"); return c.Next() }
	Mount(app.Group("/v1.0"), svc, auth, func(c fiber.Ctx) string { return c.Locals("uid").(string) })
	return app
}

func do(t *testing.T, app *fiber.App, method, path, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestDeleteBadIDIs400(t *testing.T) {
	app := newApp(t, NewService(newMemStore(), publicKey(t)))
	if res := do(t, app, "DELETE", "/v1.0/push/subscriptions/not-a-hex-id", ""); res.StatusCode != 400 {
		t.Errorf("status = %d", res.StatusCode)
	}
}

func TestSubscribeListDeleteRoundTrip(t *testing.T) {
	app := newApp(t, NewService(newMemStore(), publicKey(t)))
	in := goodSubscription(t)
	body, _ := json.Marshal(map[string]any{"endpoint": in.Endpoint, "keys": map[string]string{"p256dh": in.P256dh, "auth": in.Auth}})
	res := do(t, app, "PUT", "/v1.0/push/subscriptions", string(body))
	if res.StatusCode != 200 {
		t.Fatalf("put = %d", res.StatusCode)
	}
	var dev Device
	_ = json.NewDecoder(res.Body).Decode(&dev)
	if len(dev.ID) != 16 {
		t.Fatalf("device = %+v", dev)
	}
	list := do(t, app, "GET", "/v1.0/push/subscriptions", "")
	var got struct {
		Data []Device `json:"data"`
		Max  int      `json:"max_subscriptions"`
	}
	_ = json.NewDecoder(list.Body).Decode(&got)
	if len(got.Data) != 1 || got.Max != MaxSubscriptionsPerUser {
		t.Errorf("list = %+v", got)
	}
	if res := do(t, app, "DELETE", "/v1.0/push/subscriptions/"+dev.ID, ""); res.StatusCode != 204 {
		t.Errorf("delete = %d", res.StatusCode)
	}
	if res := do(t, app, "DELETE", "/v1.0/push/subscriptions/"+dev.ID, ""); res.StatusCode != 404 {
		t.Errorf("second delete = %d", res.StatusCode)
	}
}

func TestPutRejectsUnknownFields(t *testing.T) {
	app := newApp(t, NewService(newMemStore(), publicKey(t)))
	if res := do(t, app, "PUT", "/v1.0/push/subscriptions", `{"endpoint":"https://x","keys":{"p256dh":"a","auth":"b"},"admin":true}`); res.StatusCode != 400 {
		t.Errorf("status = %d", res.StatusCode)
	}
}
