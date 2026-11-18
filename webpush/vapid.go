package webpush

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/url"
	"time"
)

const (
	jwtLifetime = time.Hour // RFC 8292 section 2: exp at most 24 h; one hour is plenty for a single send
	coordLen    = 32
)

// ErrInvalidKey reports a VAPID key that is not a P-256 private key.
var ErrInvalidKey = errors.New("webpush: invalid VAPID key")

// Key is one VAPID key pair. Subscriptions are bound to the public key they were
// created with (applicationServerKey), so a rotation keeps the previous Key
// available under its KID until clients resubscribe.
type Key struct {
	Private *ecdsa.PrivateKey
	// Public is the 65-byte uncompressed point, the value browsers receive as applicationServerKey.
	Public []byte
	// KID is a stable short id of the public key (8 base64url characters), stored with each subscription.
	KID string
}

// PublicBase64URL is Public as the browser expects it.
func (k Key) PublicBase64URL() string { return base64.RawURLEncoding.EncodeToString(k.Public) }

// GenerateKey creates a key pair and returns it with its PEM (PKCS#8) for the secret store.
func GenerateKey() (string, Key, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", Key{}, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", Key{}, err
	}
	k, err := newKey(priv)
	if err != nil {
		return "", Key{}, err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), k, nil
}

// ParseKey reads a P-256 private key from PEM (PKCS#8 or SEC1).
func ParseKey(pemText string) (Key, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return Key{}, fmt.Errorf("%w: not PEM", ErrInvalidKey)
	}
	var priv *ecdsa.PrivateKey
	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		ec, ok := parsed.(*ecdsa.PrivateKey)
		if !ok {
			return Key{}, fmt.Errorf("%w: not an EC key", ErrInvalidKey)
		}
		priv = ec
	} else if ec, err2 := x509.ParseECPrivateKey(block.Bytes); err2 == nil {
		priv = ec
	} else {
		return Key{}, fmt.Errorf("%w: %v", ErrInvalidKey, err)
	}
	return newKey(priv)
}

func newKey(priv *ecdsa.PrivateKey) (Key, error) {
	if priv.Curve != elliptic.P256() {
		return Key{}, fmt.Errorf("%w: curve must be P-256", ErrInvalidKey)
	}
	ecdhPub, err := priv.PublicKey.ECDH()
	if err != nil {
		return Key{}, fmt.Errorf("%w: %v", ErrInvalidKey, err)
	}
	pub := ecdhPub.Bytes()
	sum := sha256.Sum256(pub)
	return Key{Private: priv, Public: pub, KID: base64.RawURLEncoding.EncodeToString(sum[:6])}, nil
}

// VAPID builds the Authorization header value of RFC 8292 for an endpoint:
// "vapid t=<jwt>, k=<public key>". The audience is the endpoint origin and the
// token lives one hour. subject is the contact URL (mailto: or https:).
func VAPID(k Key, endpoint, subject string, now time.Time) (string, error) {
	if k.Private == nil {
		return "", fmt.Errorf("%w: no private key", ErrInvalidKey)
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("%w: endpoint", ErrInvalidSubscription)
	}
	header, _ := json.Marshal(map[string]string{"typ": "JWT", "alg": "ES256"})
	claims, _ := json.Marshal(map[string]any{
		"aud": u.Scheme + "://" + u.Host,
		"exp": now.Add(jwtLifetime).Unix(),
		"sub": subject,
	})
	signing := b64(header) + "." + b64(claims)
	digest := sha256.Sum256([]byte(signing))
	sig, err := ecdsa.SignASN1(rand.Reader, k.Private, digest[:])
	if err != nil {
		return "", err
	}
	raw, err := rawSignature(sig)
	if err != nil {
		return "", err
	}
	return "vapid t=" + signing + "." + b64(raw) + ", k=" + k.PublicBase64URL(), nil
}

// rawSignature converts the DER signature to the JWS form: r and s as 32-byte
// big-endian integers, left-padded (a DER integer drops leading zeros, which
// ES256 must keep).
func rawSignature(der []byte) ([]byte, error) {
	var sig struct{ R, S *big.Int }
	if _, err := asn1.Unmarshal(der, &sig); err != nil {
		return nil, err
	}
	out := make([]byte, 2*coordLen)
	sig.R.FillBytes(out[:coordLen])
	sig.S.FillBytes(out[coordLen:])
	return out, nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// String, GoString and LogValue redact: only the KID is shown, never the private key.
func (k Key) String() string       { return "webpush.Key{kid=" + k.KID + "}" }
func (k Key) GoString() string     { return k.String() }
func (k Key) LogValue() slog.Value { return slog.StringValue(k.String()) }
