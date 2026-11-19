package pushsvc

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gofiber/fiber/v3"

	"gopkg.aoctech.app/api-commons/webpush"
)

// memStore is an in-memory SubscriptionStore.
type memStore struct {
	mu   sync.Mutex
	subs map[string]map[string]Subscription // sub -> id -> row
}

func newMemStore() *memStore { return &memStore{subs: map[string]map[string]Subscription{}} }

func (m *memStore) List(_ context.Context, sub string) ([]Subscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Subscription
	for _, s := range m.subs[sub] {
		out = append(out, s)
	}
	return out, nil
}

func (m *memStore) Upsert(_ context.Context, sub string, d Subscription) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d.ID = SubscriptionID(d.Endpoint)
	if m.subs[sub] == nil {
		m.subs[sub] = map[string]Subscription{}
	}
	m.subs[sub][d.ID] = d
	return d.ID, nil
}

func (m *memStore) Delete(_ context.Context, sub, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.subs[sub][id]; !ok {
		return ErrNotFound
	}
	delete(m.subs[sub], id)
	return nil
}

func (m *memStore) OtherOwners(_ context.Context, endpoint, sub string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := SubscriptionID(endpoint)
	var out []string
	for owner, rows := range m.subs {
		if _, ok := rows[id]; ok && owner != sub {
			out = append(out, owner)
		}
	}
	return out, nil
}

// goodSubscription is a valid browser subscription: a real P-256 point and a 16-byte secret.
func goodSubscription(t *testing.T) SubscriptionInput {
	t.Helper()
	_, key, err := webpush.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return SubscriptionInput{
		Endpoint: "https://fcm.googleapis.com/fcm/send/abc", P256dh: key.PublicBase64URL(),
		Auth: "AAAAAAAAAAAAAAAAAAAAAA", UserAgent: "Mozilla/5.0 (Linux; Android 14) Chrome/120",
	}
}

func publicKey(t *testing.T) string {
	t.Helper()
	_, k, err := webpush.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k.PublicBase64URL()
}

func TestSubscribeRejectsHTTPEndpoint(t *testing.T) {
	svc := NewService(newMemStore(), publicKey(t))
	in := goodSubscription(t)
	in.Endpoint = "http://fcm.googleapis.com/fcm/send/abc"
	_, err := svc.Subscribe(context.Background(), "u1", in)
	if err == nil || !strings.Contains(err.Error(), "inválida") {
		t.Fatalf("err = %v, want a bad-request problem", err)
	}
}

func TestSubscribeMovesEndpointFromPreviousOwner(t *testing.T) {
	st := newMemStore()
	svc := NewService(st, publicKey(t))
	in := goodSubscription(t)
	if _, err := svc.Subscribe(context.Background(), "alice", in); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Subscribe(context.Background(), "bob", in); err != nil {
		t.Fatal(err)
	}
	if a, _ := st.List(context.Background(), "alice"); len(a) != 0 {
		t.Errorf("alice still holds the endpoint: %v", a)
	}
	if b, _ := st.List(context.Background(), "bob"); len(b) != 1 {
		t.Errorf("bob = %v", b)
	}
}

func TestSubscribeWhenDisabledIsServiceUnavailable(t *testing.T) {
	svc := NewService(newMemStore(), "")
	_, err := svc.Subscribe(context.Background(), "u1", goodSubscription(t))
	var p interface{ Error() string }
	if !errors.As(err, &p) || !strings.Contains(err.Error(), "desativadas") {
		t.Fatalf("err = %v", err)
	}
}

func TestConfigDisabledIsNotAnError(t *testing.T) {
	c := NewService(newMemStore(), "").Config()
	if c.Enabled || c.PublicKey != "" || c.MaxSubscriptions != MaxSubscriptionsPerUser {
		t.Errorf("config = %+v", c)
	}
	on := NewService(newMemStore(), publicKey(t)).Config()
	if !on.Enabled || on.PublicKey == "" || on.VapidKID == "" {
		t.Errorf("config = %+v", on)
	}
}

func TestDeviceLabel(t *testing.T) {
	for ua, want := range map[string]string{
		"Mozilla/5.0 (Linux; Android 14) Chrome/120 Mobile Safari/537":         "Chrome no Android",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17) AppleWebKit Version/17 Safari": "Safari no iOS",
		"Mozilla/5.0 (X11; Linux x86_64; rv:120.0) Gecko/20100101 Firefox/120": "Firefox no Linux",
		"Mozilla/5.0 (Windows NT 10.0) Chrome/120 Safari/537 Edg/120":          "Edge no Windows",
		"": "Navegador",
	} {
		if got := DeviceLabel(ua); got != want {
			t.Errorf("DeviceLabel(%q) = %q, want %q", ua, got, want)
		}
	}
}

// An endpoint is a secret capability URL. A request that names it with keys of its own is not the browser that owns it:
// it must not evict the owner, and it must not block the owner either (whoever registers an endpoint first would
// otherwise lock its real owner out). A row with the wrong keys only produces a push the browser cannot decrypt.
func TestSubscribeWithDifferentKeysNeverEvictsTheOwnerNorBlocksTheCaller(t *testing.T) {
	ctx := context.Background()
	for _, attackerFirst := range []bool{false, true} {
		st := newMemStore()
		svc := NewService(st, publicKey(t))
		owner := goodSubscription(t)
		attacker := goodSubscription(t) // same endpoint, other P-256 key and secret
		attacker.Auth = "BBBBBBBBBBBBBBBBBBBBBB"
		first, second := struct {
			who string
			in  SubscriptionInput
		}{"alice", owner}, struct {
			who string
			in  SubscriptionInput
		}{"mallory", attacker}
		if attackerFirst {
			first, second = second, first
		}
		for _, r := range []struct {
			who string
			in  SubscriptionInput
		}{first, second} {
			if _, err := svc.Subscribe(ctx, r.who, r.in); err != nil {
				t.Fatalf("attackerFirst=%v: %s was refused: %v", attackerFirst, r.who, err)
			}
		}
		if a, _ := st.List(ctx, "alice"); len(a) != 1 {
			t.Errorf("attackerFirst=%v: alice's subscription = %v, want it kept or created", attackerFirst, a)
		}
	}
}

func TestMountRefusesACallerWithoutAnId(t *testing.T) {
	app := fiber.New()
	Mount(app.Group("/v1.0"), NewService(newMemStore(), publicKey(t)), func(c fiber.Ctx) error { return c.Next() },
		func(fiber.Ctx) string { return "" })
	req := httptest.NewRequest("GET", "/v1.0/push/subscriptions", nil)
	res, err := app.Test(req)
	if err != nil || res.StatusCode != 401 {
		t.Errorf("status = %v, %v: an empty id would address the shared partition USER#", res.StatusCode, err)
	}
}

func TestSubscribeCallsTheOnSubscribeHookAfterTheRowIsStored(t *testing.T) {
	st := newMemStore()
	var got []string
	svc := NewService(st, publicKey(t)).WithOnSubscribe(func(_ context.Context, sub string) error {
		rows, _ := st.List(context.Background(), sub)
		got = append(got, sub+":"+string(rune('0'+len(rows))))
		return nil
	})
	if _, err := svc.Subscribe(context.Background(), "u1", goodSubscription(t)); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "u1:1" {
		t.Errorf("hook calls = %v, want one call after the row exists", got)
	}
	failing := NewService(newMemStore(), publicKey(t)).WithOnSubscribe(func(context.Context, string) error { return errors.New("boom") })
	if _, err := failing.Subscribe(context.Background(), "u1", goodSubscription(t)); err == nil {
		t.Error("a failing hook must fail the subscribe so the client retries")
	}
}
