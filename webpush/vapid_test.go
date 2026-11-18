package webpush

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

func TestVAPIDHeaderAndSignature(t *testing.T) {
	_, key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	for _, endpoint := range []string{
		"https://fcm.googleapis.com/fcm/send/abc",
		"https://updates.push.services.mozilla.com:443/wpush/v2/xyz",
		"https://web.push.apple.com:8443/some/path?x=1",
	} {
		hdr, err := VAPID(key, endpoint, "mailto:ops@example.com", now)
		if err != nil {
			t.Fatal(err)
		}
		rest, ok := strings.CutPrefix(hdr, "vapid t=")
		if !ok {
			t.Fatalf("header = %q", hdr)
		}
		jwt, pub, ok := strings.Cut(rest, ", k=")
		if !ok || pub != key.PublicBase64URL() {
			t.Fatalf("k= part wrong: %q", hdr)
		}
		parts := strings.Split(jwt, ".")
		if len(parts) != 3 {
			t.Fatalf("jwt parts = %d", len(parts))
		}
		var header map[string]string
		if err := json.Unmarshal(mustB64(t, parts[0]), &header); err != nil || header["alg"] != "ES256" || header["typ"] != "JWT" {
			t.Fatalf("header = %v (%v)", header, err)
		}
		var claims struct {
			Aud string `json:"aud"`
			Exp int64  `json:"exp"`
			Sub string `json:"sub"`
		}
		if err := json.Unmarshal(mustB64(t, parts[1]), &claims); err != nil {
			t.Fatal(err)
		}
		wantAud := endpoint[:strings.Index(endpoint[8:], "/")+8]
		if claims.Aud != wantAud || claims.Sub != "mailto:ops@example.com" || claims.Exp != now.Add(time.Hour).Unix() {
			t.Errorf("claims = %+v, want aud %s", claims, wantAud)
		}
		verifyES256(t, key, parts)
	}
}

// verifyES256 checks the raw r||s signature (RFC 7518 section 3.4), not DER.
func verifyES256(t *testing.T, key Key, parts []string) {
	t.Helper()
	sig := mustB64(t, parts[2])
	if len(sig) != 64 {
		t.Fatalf("signature is %d bytes, want 64 (r||s)", len(sig))
	}
	der, err := asn1.Marshal(struct{ R, S *big.Int }{new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.VerifyASN1(&key.Private.PublicKey, digest[:], der) {
		t.Fatal("signature does not verify")
	}
}

// A DER integer drops leading zero bytes; ES256 must pad them back. Enough signatures that a short
// r or s shows up (about 1 in 128 per signature).
func TestVAPIDSignatureAlwaysFixedWidth(t *testing.T) {
	_, key, _ := GenerateKey()
	for i := 0; i < 600; i++ {
		hdr, err := VAPID(key, "https://fcm.googleapis.com/x", "mailto:a@b.c", time.Unix(int64(i), 0))
		if err != nil {
			t.Fatal(err)
		}
		jwt, _, _ := strings.Cut(strings.TrimPrefix(hdr, "vapid t="), ", k=")
		parts := strings.Split(jwt, ".")
		verifyES256(t, key, parts)
	}
}

func TestParseKeyFormats(t *testing.T) {
	pkcs8, key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseKey(pkcs8)
	if err != nil || got.KID != key.KID || got.PublicBase64URL() != key.PublicBase64URL() {
		t.Fatalf("PKCS8: %v %+v", err, got)
	}
	sec1, err := x509.MarshalECPrivateKey(key.Private)
	if err != nil {
		t.Fatal(err)
	}
	got, err = ParseKey(string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1})))
	if err != nil || got.KID != key.KID {
		t.Fatalf("SEC1: %v %+v", err, got)
	}
	if len(key.Public) != 65 || len(key.KID) != 8 {
		t.Errorf("public %d bytes, kid %q", len(key.Public), key.KID)
	}
	for _, bad := range []string{"", "not pem", "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----"} {
		if _, err := ParseKey(bad); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("ParseKey(%q) err = %v", bad, err)
		}
	}
}

func TestVAPIDRejectsBadInput(t *testing.T) {
	if _, err := VAPID(Key{}, "https://fcm.googleapis.com/x", "mailto:a@b.c", time.Now()); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("zero key: %v", err)
	}
	_, key, _ := GenerateKey()
	if _, err := VAPID(key, "not a url", "mailto:a@b.c", time.Now()); err == nil {
		t.Error("bad endpoint accepted")
	}
}
