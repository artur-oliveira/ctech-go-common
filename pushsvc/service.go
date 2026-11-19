package pushsvc

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"

	"gopkg.aoctech.app/api-commons/problem"
	"gopkg.aoctech.app/api-commons/webpush"
)

// vapidPoint is the length of an uncompressed P-256 point, the form of a VAPID public key.
const vapidPoint = 65

var errOff = problem.ServiceUnavailable("notificações desativadas nesta instalação")

// SubscriptionStore is what Service needs of storage; *Store implements it.
type SubscriptionStore interface {
	List(ctx context.Context, sub string) ([]Subscription, error)
	Upsert(ctx context.Context, sub string, d Subscription) (string, error)
	Delete(ctx context.Context, sub, id string) error
	OtherOwners(ctx context.Context, endpoint, sub string) ([]string, error)
}

var _ SubscriptionStore = (*Store)(nil)

// Service is the subscribe side of Web Push for one product.
type Service struct {
	store     SubscriptionStore
	publicKey string
	kid       string
}

// NewService returns a Service. An empty or malformed publicKey leaves push off: Config says so and Subscribe refuses.
func NewService(store SubscriptionStore, publicKey string) *Service {
	s := &Service{store: store}
	if kid, ok := vapidKID(publicKey); ok {
		s.publicKey, s.kid = publicKey, kid
	}
	return s
}

func vapidKID(publicKey string) (string, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(publicKey)
	if err != nil || len(raw) != vapidPoint || raw[0] != 4 {
		return "", false
	}
	sum := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(sum[:6]), true
}

// Enabled reports whether push can be used at all.
func (s *Service) Enabled() bool { return s != nil && s.publicKey != "" }

// Config is what the browser needs to subscribe. Disabled is a normal answer, not an error: the screen hides itself.
type Config struct {
	Enabled          bool   `json:"enabled"`
	PublicKey        string `json:"public_key,omitempty"`
	VapidKID         string `json:"vapid_kid,omitempty"`
	MaxSubscriptions int    `json:"max_subscriptions"`
}

// Config answers GET /push/config.
func (s *Service) Config() Config {
	c := Config{Enabled: s.Enabled(), MaxSubscriptions: MaxSubscriptionsPerUser}
	if c.Enabled {
		c.PublicKey, c.VapidKID = s.publicKey, s.kid
	}
	return c
}

// SubscriptionInput is a browser's PushSubscription.toJSON().
type SubscriptionInput struct {
	Endpoint, P256dh, Auth, UserAgent string
}

// Device is one device as the screen shows it (never the endpoint).
type Device struct {
	ID         string `json:"id"`
	Device     string `json:"device"`
	CreatedAt  string `json:"created_at"`
	LastSeenAt string `json:"last_seen_at"`
}

// Subscribe registers or renews the person's device. The endpoint is validated by the parser the sender uses: https, a
// known push-service host, a valid P-256 key and a 16-byte secret. One browser endpoint belongs to one person: whoever
// held it before loses it now, and a failure there aborts the subscription instead of leaving two owners.
func (s *Service) Subscribe(ctx context.Context, sub string, in SubscriptionInput) (*Device, error) {
	if !s.Enabled() {
		return nil, errOff
	}
	if _, err := webpush.ParseSubscription(in.Endpoint, in.P256dh, in.Auth); err != nil {
		return nil, problem.BadRequest("inscrição de notificação inválida")
	}
	owners, err := s.store.OtherOwners(ctx, in.Endpoint, sub)
	if err != nil {
		return nil, err
	}
	id := SubscriptionID(in.Endpoint)
	for _, previous := range owners {
		// The move is for a shared browser: the new person's browser holds the same subscription, so it presents the
		// same keys. A request that names the endpoint with other keys is not that browser and must not evict its owner.
		held, err := s.store.List(ctx, previous)
		if err != nil {
			return nil, err
		}
		for _, h := range held {
			if h.ID == id && !sameKeys(h, in) {
				return nil, problem.Conflict("este dispositivo já pertence a outra conta")
			}
		}
		if err := s.store.Delete(ctx, previous, id); err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	label := DeviceLabel(in.UserAgent)
	id, err = s.store.Upsert(ctx, sub, Subscription{Endpoint: in.Endpoint, P256dh: in.P256dh, Auth: in.Auth, VapidKID: s.kid, Device: label})
	if err != nil {
		return nil, err
	}
	return &Device{ID: id, Device: label}, nil
}

// sameKeys compares the subscription keys in constant time: auth is a secret.
func sameKeys(h Subscription, in SubscriptionInput) bool {
	return subtle.ConstantTimeCompare([]byte(h.P256dh), []byte(in.P256dh)) == 1 &&
		subtle.ConstantTimeCompare([]byte(h.Auth), []byte(in.Auth)) == 1
}

// Devices lists the person's devices.
func (s *Service) Devices(ctx context.Context, sub string) ([]Device, error) {
	list, err := s.store.List(ctx, sub)
	if err != nil {
		return nil, err
	}
	out := make([]Device, 0, len(list))
	for _, d := range list {
		out = append(out, Device{ID: d.ID, Device: d.Device, CreatedAt: d.CreatedAt, LastSeenAt: d.LastSeenAt})
	}
	return out, nil
}

// Unsubscribe removes one of the person's devices.
func (s *Service) Unsubscribe(ctx context.Context, sub, id string) error {
	err := s.store.Delete(ctx, sub, id)
	if errors.Is(err, ErrNotFound) {
		return problem.NotFound("dispositivo não encontrado")
	}
	return err
}
