package webpush

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

// rfcVector is testdata/rfc8291.json: the values of RFC 8291 section 5 and appendix A, extracted
// from the RFC text by script (never typed by hand).
type rfcVector struct {
	PlaintextB64 string `json:"plaintext_b64"`
	ASPublic     string `json:"as_public"`
	ASPrivate    string `json:"as_private"`
	UAPublic     string `json:"ua_public"`
	UAPrivate    string `json:"ua_private"`
	Salt         string `json:"salt"`
	AuthSecret   string `json:"auth_secret"`
	ECDHSecret   string `json:"ecdh_secret"`
	PRKKey       string `json:"prk_key"`
	IKM          string `json:"ikm"`
	PRK          string `json:"prk"`
	CEK          string `json:"cek"`
	Nonce        string `json:"nonce"`
	Body         string `json:"body"`
}

func loadVector(t *testing.T) rfcVector {
	t.Helper()
	raw, err := os.ReadFile("testdata/rfc8291.json")
	if err != nil {
		t.Fatal(err)
	}
	var v rfcVector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := decodeB64(s)
	if err != nil {
		t.Fatalf("base64url %q: %v", s, err)
	}
	return b
}

func TestEncryptMatchesRFC8291Vector(t *testing.T) {
	v := loadVector(t)
	asPriv, err := ecdh.P256().NewPrivateKey(mustB64(t, v.ASPrivate))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(asPriv.PublicKey().Bytes(), mustB64(t, v.ASPublic)) {
		t.Fatal("as_private does not give as_public")
	}
	sub := Subscription{Endpoint: "https://push.example.net/push/JzLQ3raZJfFBR0aqvOMsLrt54w4rJUsV", P256dh: mustB64(t, v.UAPublic), Auth: mustB64(t, v.AuthSecret)}
	plaintext := mustB64(t, v.PlaintextB64)
	if string(plaintext) != "When I grow up, I want to be a watermelon" {
		t.Fatalf("plaintext = %q", plaintext)
	}

	// Intermediate values (appendix A).
	ua, _ := ecdh.P256().NewPublicKey(sub.P256dh)
	secret, err := asPriv.ECDH(ua)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(secret, mustB64(t, v.ECDHSecret)) {
		t.Error("ecdh_secret differs from the RFC")
	}
	cek, nonce, err := deriveKeys(secret, sub.Auth, sub.P256dh, asPriv.PublicKey().Bytes(), mustB64(t, v.Salt))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cek, mustB64(t, v.CEK)) || !bytes.Equal(nonce, mustB64(t, v.Nonce)) {
		t.Error("CEK or NONCE differs from the RFC")
	}

	got, err := encrypt(sub, plaintext, asPriv, mustB64(t, v.Salt))
	if err != nil {
		t.Fatal(err)
	}
	if want := mustB64(t, v.Body); !bytes.Equal(got, want) {
		t.Fatalf("body differs from RFC 8291 section 5\n got %x\nwant %x", got, want)
	}
}

// decrypt is the user agent side: it opens a body with the receiver private key, enough to prove that
// what Encrypt writes is what a browser reads.
func decrypt(t *testing.T, body, uaPriv, auth []byte) []byte {
	t.Helper()
	if len(body) < headerLen+17 {
		t.Fatalf("body too short: %d", len(body))
	}
	salt := body[:saltLen]
	if rs := binary.BigEndian.Uint32(body[saltLen : saltLen+4]); rs != RecordSize {
		t.Fatalf("rs = %d", rs)
	}
	if idlen := body[saltLen+4]; idlen != pubKeyLen {
		t.Fatalf("idlen = %d", idlen)
	}
	asPubBytes := body[saltLen+5 : headerLen]
	priv, err := ecdh.P256().NewPrivateKey(uaPriv)
	if err != nil {
		t.Fatal(err)
	}
	asPub, err := ecdh.P256().NewPublicKey(asPubBytes)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := priv.ECDH(asPub)
	if err != nil {
		t.Fatal(err)
	}
	cek, nonce, err := deriveKeys(secret, auth, priv.PublicKey().Bytes(), asPubBytes, salt)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	record, err := gcm.Open(nil, nonce, body[headerLen:], nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Strip zero padding, then the delimiter of the last record.
	i := len(record) - 1
	for i >= 0 && record[i] == 0 {
		i--
	}
	if i < 0 || record[i] != lastRecordFlag {
		t.Fatalf("missing last-record delimiter")
	}
	return record[:i]
}

func newTestSubscription(t *testing.T) (Subscription, []byte) {
	t.Helper()
	ua, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, authLen)
	_, _ = rand.Read(auth)
	return Subscription{Endpoint: "https://fcm.googleapis.com/fcm/send/abc", P256dh: ua.PublicKey().Bytes(), Auth: auth}, ua.Bytes()
}

func TestEncryptRoundTrip(t *testing.T) {
	sub, uaPriv := newTestSubscription(t)
	for _, n := range []int{0, 1, 1024, MaxPlaintext} {
		plain := bytes.Repeat([]byte{'x'}, n)
		body, err := Encrypt(sub, plain)
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if len(body) > RecordSize {
			t.Errorf("n=%d: body %d exceeds %d", n, len(body), RecordSize)
		}
		if got := decrypt(t, body, uaPriv, sub.Auth); !bytes.Equal(got, plain) {
			t.Errorf("n=%d: round trip differs", n)
		}
	}
}

func TestEncryptIsRandomised(t *testing.T) {
	sub, _ := newTestSubscription(t)
	a, _ := Encrypt(sub, []byte("same"))
	b, _ := Encrypt(sub, []byte("same"))
	if bytes.Equal(a, b) {
		t.Error("two encryptions of the same payload are identical: salt or ephemeral key reused")
	}
}

func TestEncryptRejects(t *testing.T) {
	sub, _ := newTestSubscription(t)
	if _, err := Encrypt(sub, make([]byte, MaxPlaintext+1)); !errors.Is(err, ErrPayloadTooLarge) {
		t.Errorf("MaxPlaintext+1: err = %v", err)
	}
	bad := sub
	bad.Auth = sub.Auth[:15]
	if _, err := Encrypt(bad, []byte("x")); !errors.Is(err, ErrInvalidSubscription) {
		t.Errorf("short auth: err = %v", err)
	}
	bad = sub
	bad.P256dh = sub.P256dh[:64]
	if _, err := Encrypt(bad, []byte("x")); !errors.Is(err, ErrInvalidSubscription) {
		t.Errorf("short key: err = %v", err)
	}
}
