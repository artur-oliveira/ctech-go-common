package pushsvc

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

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

// An endpoint is a secret capability URL, but whoever learns one must not be able to evict its owner by registering it
// with keys of their own: the legitimate move (a shared browser, a new person signed in) carries the same keys.
func TestSubscribeRefusesToTakeAnEndpointWithDifferentKeys(t *testing.T) {
	st := newMemStore()
	svc := NewService(st, publicKey(t))
	in := goodSubscription(t)
	if _, err := svc.Subscribe(context.Background(), "alice", in); err != nil {
		t.Fatal(err)
	}
	attacker := goodSubscription(t) // same endpoint, other P-256 key
	attacker.Auth = "BBBBBBBBBBBBBBBBBBBBBB"
	if _, err := svc.Subscribe(context.Background(), "mallory", attacker); err == nil || !strings.Contains(err.Error(), "outra conta") {
		t.Fatalf("err = %v, want a conflict", err)
	}
	if a, _ := st.List(context.Background(), "alice"); len(a) != 1 {
		t.Errorf("alice lost her subscription: %v", a)
	}
	if m, _ := st.List(context.Background(), "mallory"); len(m) != 0 {
		t.Errorf("mallory got a row: %v", m)
	}
}
