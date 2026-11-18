package webpush

import (
	"crypto/ecdh"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Limits and defaults (RFC 8291 section 4 and RFC 8030).
const (
	// MaxPlaintext is the largest payload a push service must accept: 4096
	// octets of body minus the 86-octet header and the 17 octets of delimiter
	// and GCM tag.
	MaxPlaintext = 3993
	// RecordSize is the aes128gcm record size (rs) announced in the header.
	RecordSize = 4096
	// DefaultTimeout bounds one delivery attempt.
	DefaultTimeout = 10 * time.Second
	// MaxEndpointLen caps a stored endpoint.
	MaxEndpointLen = 2048

	authLen   = 16
	pubKeyLen = 65
	maxTopic  = 32
)

// AllowedHosts are the push services a subscription endpoint may point at
// (suffix match on a dot boundary, https only). It is the second barrier after
// the guarded dialer: a tampered endpoint cannot name an arbitrary host.
var AllowedHosts = []string{
	"fcm.googleapis.com",
	"updates.push.services.mozilla.com",
	"push.apple.com",
	"notify.windows.com",
}

var (
	// ErrInvalidSubscription reports an endpoint or key a browser could not have produced.
	ErrInvalidSubscription = errors.New("webpush: invalid subscription")
	// ErrPayloadTooLarge reports a payload above MaxPlaintext, before any network call.
	ErrPayloadTooLarge = errors.New("webpush: payload too large")
)

// Subscription is one browser subscription (PushSubscription.toJSON).
type Subscription struct {
	Endpoint string
	// P256dh is the 65-byte uncompressed P-256 public key of the user agent.
	P256dh []byte
	// Auth is the 16-byte authentication secret.
	Auth []byte
}

// ParseSubscription validates what the browser sent at subscribe time: an https
// endpoint (no credentials, default port) on an allowed push service, the
// p256dh key as a valid P-256 point and a 16-byte auth secret. Keys are
// base64url, with or without padding.
func ParseSubscription(endpoint, p256dh, auth string) (Subscription, error) {
	if len(endpoint) == 0 || len(endpoint) > MaxEndpointLen {
		return Subscription{}, fmt.Errorf("%w: endpoint length", ErrInvalidSubscription)
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return Subscription{}, fmt.Errorf("%w: endpoint is not a plain https URL", ErrInvalidSubscription)
	}
	if p := u.Port(); p != "" && p != "443" {
		return Subscription{}, fmt.Errorf("%w: endpoint port", ErrInvalidSubscription)
	}
	if !hostAllowed(u.Hostname()) {
		return Subscription{}, fmt.Errorf("%w: host %q is not an allowed push service", ErrInvalidSubscription, u.Hostname())
	}
	pub, err := decodeB64(p256dh)
	if err != nil {
		return Subscription{}, fmt.Errorf("%w: p256dh encoding", ErrInvalidSubscription)
	}
	if _, err := ecdh.P256().NewPublicKey(pub); err != nil || len(pub) != pubKeyLen {
		return Subscription{}, fmt.Errorf("%w: p256dh is not a P-256 point", ErrInvalidSubscription)
	}
	sec, err := decodeB64(auth)
	if err != nil || len(sec) != authLen {
		return Subscription{}, fmt.Errorf("%w: auth must be %d bytes", ErrInvalidSubscription, authLen)
	}
	return Subscription{Endpoint: endpoint, P256dh: pub, Auth: sec}, nil
}

func hostAllowed(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, allowed := range AllowedHosts {
		if host == allowed || strings.HasSuffix(host, "."+allowed) {
			return true
		}
	}
	return false
}

func decodeB64(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}
