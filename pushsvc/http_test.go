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

// The body is read strictly and bounded like the validator used to: these pin the behavior so the dependency can go (the
// validator pulls mimetype, x/text and friends, about 1.5 MB, into every binary of a product that imports pushsvc).
func TestPutSubscriptionRejectsBadBodies(t *testing.T) {
	app := newApp(t, NewService(newMemStore(), publicKey(t)))
	long := strings.Repeat("a", 2049)
	const ok = `"endpoint":"https://fcm.googleapis.com/x","keys":{"p256dh":"a","auth":"b"}`
	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"no keys":            {`{"endpoint":"https://fcm.googleapis.com/x"}`, 422},
		"no endpoint":        {`{"keys":{"p256dh":"a","auth":"b"}}`, 422},
		"no p256dh":          {`{"endpoint":"https://fcm.googleapis.com/x","keys":{"auth":"b"}}`, 422},
		"no auth":            {`{"endpoint":"https://fcm.googleapis.com/x","keys":{"p256dh":"a"}}`, 422},
		"endpoint too long":  {`{"endpoint":"` + long + `","keys":{"p256dh":"a","auth":"b"}}`, 422},
		"p256dh too long":    {`{"endpoint":"https://fcm.googleapis.com/x","keys":{"p256dh":"` + strings.Repeat("a", 129) + `","auth":"b"}}`, 422},
		"auth too long":      {`{"endpoint":"https://fcm.googleapis.com/x","keys":{"p256dh":"a","auth":"` + strings.Repeat("a", 65) + `"}}`, 422},
		"content after json": {`{` + ok + `} {}`, 400},
		"not json":           {`endpoint=x`, 400},
		"unknown field":      {`{` + ok + `,"pad":1}`, 400},
	} {
		if res := do(t, app, "PUT", "/v1.0/push/subscriptions", tc.body); res.StatusCode != tc.want {
			t.Errorf("%s: status = %d, want %d", name, res.StatusCode, tc.want)
		}
	}
}

// A subscription body is a few hundred bytes; anything past the limit is refused before it is parsed.
func TestPutSubscriptionRefusesABodyOverTheLimit(t *testing.T) {
	app := newApp(t, NewService(newMemStore(), publicKey(t)))
	body := `{"endpoint":"https://fcm.googleapis.com/x","keys":{"p256dh":"a","auth":"b"},"pad":"` + strings.Repeat("a", maxBodyBytes) + `"}`
	if res := do(t, app, "PUT", "/v1.0/push/subscriptions", body); res.StatusCode != 413 {
		t.Errorf("status = %d, want 413", res.StatusCode)
	}
}
