package webpush

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func validKeys(t testing.TB) (p256dh, auth string) {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	a := make([]byte, authLen)
	_, _ = rand.Read(a)
	return base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()), base64.RawURLEncoding.EncodeToString(a)
}

func TestParseSubscription(t *testing.T) {
	p, a := validKeys(t)
	ok := []string{
		"https://fcm.googleapis.com/fcm/send/abc",
		"https://updates.push.services.mozilla.com/wpush/v2/abc",
		"https://web.push.apple.com/abc",
		"https://wns2-par02p.notify.windows.com/w/?token=abc",
		"https://fcm.googleapis.com:443/fcm/send/abc",
	}
	for _, e := range ok {
		if _, err := ParseSubscription(e, p, a); err != nil {
			t.Errorf("%s: %v", e, err)
		}
	}
	// Padded base64url is accepted too.
	if _, err := ParseSubscription(ok[0], p+"=", a+"=="); err != nil {
		t.Errorf("padded keys: %v", err)
	}

	bad := map[string]string{
		"http":               "http://fcm.googleapis.com/fcm/send/abc",
		"host outside list":  "https://evil.example.com/fcm/send/abc",
		"suffix trick":       "https://notfcm.googleapis.com.evil.com/x",
		"lookalike":          "https://evilfcm.googleapis.com/x",
		"credentials":        "https://user:pw@fcm.googleapis.com/x",
		"odd port":           "https://fcm.googleapis.com:8443/x",
		"ip literal":         "https://127.0.0.1/x",
		"empty":              "",
		"no scheme":          "fcm.googleapis.com/x",
		"too long":           "https://fcm.googleapis.com/" + strings.Repeat("a", MaxEndpointLen),
		"userinfo no secret": "https://@fcm.googleapis.com/x",
	}
	for name, e := range bad {
		if _, err := ParseSubscription(e, p, a); !errors.Is(err, ErrInvalidSubscription) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := ParseSubscription(ok[0], "AAAA", a); !errors.Is(err, ErrInvalidSubscription) {
		t.Errorf("short p256dh: %v", err)
	}
	notOnCurve := base64.RawURLEncoding.EncodeToString(append([]byte{4}, make([]byte, 64)...))
	if _, err := ParseSubscription(ok[0], notOnCurve, a); !errors.Is(err, ErrInvalidSubscription) {
		t.Errorf("point off the curve: %v", err)
	}
	short := base64.RawURLEncoding.EncodeToString(make([]byte, 15))
	if _, err := ParseSubscription(ok[0], p, short); !errors.Is(err, ErrInvalidSubscription) {
		t.Errorf("15-byte auth: %v", err)
	}
	if _, err := ParseSubscription(ok[0], "!!!", a); !errors.Is(err, ErrInvalidSubscription) {
		t.Errorf("bad encoding: %v", err)
	}
}

func FuzzParseSubscription(f *testing.F) {
	p, a := validKeys(f)
	f.Add("https://fcm.googleapis.com/fcm/send/abc", p, a)
	f.Add("http://x", "", "")
	f.Fuzz(func(t *testing.T, endpoint, p256dh, auth string) {
		sub, err := ParseSubscription(endpoint, p256dh, auth)
		if err != nil {
			if !errors.Is(err, ErrInvalidSubscription) {
				t.Fatalf("unexpected error type: %v", err)
			}
			return
		}
		// Anything accepted must be usable: the right sizes, an allowed host, and encryptable.
		if len(sub.P256dh) != pubKeyLen || len(sub.Auth) != authLen || len(sub.Endpoint) > MaxEndpointLen {
			t.Fatalf("accepted a malformed subscription: %+v", sub)
		}
		if _, err := Encrypt(sub, []byte("x")); err != nil {
			t.Fatalf("accepted subscription cannot encrypt: %v", err)
		}
	})
}
