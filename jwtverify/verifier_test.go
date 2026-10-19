package jwtverify_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"gopkg.aoctech.app/api-commons/cache"
	"gopkg.aoctech.app/api-commons/jwtverify"
)

const (
	testIssuer   = "https://accounts.example"
	testAudience = "https://api.example"
)

// jwksServer serves a JWKS whose key set can be swapped at runtime, simulating
// a key rotation at the identity provider.
type jwksServer struct {
	srv    *httptest.Server
	keys   atomic.Value // []map[string]any
	hits   atomic.Int64
	status atomic.Int64
}

func newJWKSServer(t *testing.T) *jwksServer {
	t.Helper()
	js := &jwksServer{}
	js.status.Store(http.StatusOK)
	js.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		js.hits.Add(1)
		code := int(js.status.Load())
		if code != http.StatusOK {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
			return
		}
		keys, _ := js.keys.Load().([]map[string]any)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	}))
	t.Cleanup(js.srv.Close)
	return js
}

func (js *jwksServer) publish(pub *rsa.PublicKey, kid string) {
	js.keys.Store([]map[string]any{{
		"kty": "RSA",
		"kid": kid,
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}})
}

func signToken(t *testing.T, key *rsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	signed, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("signing token: %v", err)
	}
	return signed
}

func (js *jwksServer) publishEC(pub *ecdsa.PublicKey, kid string) {
	size := (pub.Curve.Params().BitSize + 7) / 8
	x := make([]byte, size)
	y := make([]byte, size)
	pub.X.FillBytes(x)
	pub.Y.FillBytes(y)
	js.keys.Store([]map[string]any{{
		"kty": "EC",
		"kid": kid,
		"crv": "P-256",
		"x":   base64.RawURLEncoding.EncodeToString(x),
		"y":   base64.RawURLEncoding.EncodeToString(y),
	}})
}

func signES256Token(t *testing.T, key *ecdsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = kid
	signed, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("signing token: %v", err)
	}
	return signed
}

func baseClaims(sub, issuer, audience string) jwt.MapClaims {
	return jwt.MapClaims{
		"sub":       sub,
		"iss":       issuer,
		"aud":       []string{audience},
		"iat":       time.Now().Unix(),
		"exp":       time.Now().Add(time.Hour).Unix(),
		"token_use": "access",
	}
}

func newVerifier(js *jwksServer) *jwtverify.Verifier {
	return jwtverify.NewVerifier(js.srv.URL, testAudience, testIssuer, cache.NewMemoryBackend(16))
}

func TestVerifyClaims_ValidToken(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	js := newJWKSServer(t)
	js.publish(&key.PublicKey, "kid-1")

	cl, err := newVerifier(js).VerifyClaims(context.Background(), signToken(t, key, "kid-1", baseClaims("user-1", testIssuer, testAudience)))
	if err != nil {
		t.Fatalf("expected valid token, got %v", err)
	}
	if cl.Sub != "user-1" {
		t.Errorf("expected sub user-1, got %q", cl.Sub)
	}
}

func TestVerifyClaims_ExtractsAllFields(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	js := newJWKSServer(t)
	js.publish(&key.PublicKey, "kid-1")
	v := newVerifier(js)

	now := time.Now().Unix()
	claims := baseClaims("user_1", testIssuer, testAudience)
	claims["scope"] = "openid internal:wallet:credit"
	claims["sid"] = "sess_1"
	claims["azp"] = "poker"
	claims["kyc_level"] = "verified"
	claims["last_mfa_at"] = now

	cl, err := v.VerifyClaims(context.Background(), signToken(t, key, "kid-1", claims))
	if err != nil {
		t.Fatalf("VerifyClaims: %v", err)
	}
	if cl.Sub != "user_1" || cl.SID != "sess_1" || cl.AZP != "poker" || cl.KYCLevel != "verified" || cl.LastMFAAt != now {
		t.Fatalf("bad claims: %+v", cl)
	}
	if !cl.HasScope("internal:wallet:credit") || cl.HasScope("internal:wallet:debit") {
		t.Fatalf("scope parsing wrong: %q", cl.Scope)
	}
}

// A token from an unexpected issuer must be rejected even though the signature
// verifies against a published key.
func TestVerifyClaims_WrongIssuer(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	js := newJWKSServer(t)
	js.publish(&key.PublicKey, "kid-1")

	_, err := newVerifier(js).VerifyClaims(context.Background(),
		signToken(t, key, "kid-1", baseClaims("user-1", "https://evil.example", testAudience)))
	if err == nil {
		t.Fatal("expected token with wrong iss to be rejected")
	}
}

func TestVerifyClaims_WrongAudience(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	js := newJWKSServer(t)
	js.publish(&key.PublicKey, "kid-1")

	_, err := newVerifier(js).VerifyClaims(context.Background(),
		signToken(t, key, "kid-1", baseClaims("user-1", testIssuer, "https://other.example")))
	if err == nil {
		t.Fatal("expected token with wrong aud to be rejected")
	}
}

// An unknown kid must be rejected outright — never verified against keys[0].
func TestVerifyClaims_UnknownKID_Rejected(t *testing.T) {
	signing, _ := rsa.GenerateKey(rand.Reader, 2048)
	published, _ := rsa.GenerateKey(rand.Reader, 2048)
	js := newJWKSServer(t)
	js.publish(&published.PublicKey, "kid-published")

	_, err := newVerifier(js).VerifyClaims(context.Background(),
		signToken(t, signing, "kid-unknown", baseClaims("user-1", testIssuer, testAudience)))
	if err == nil {
		t.Fatal("expected unknown kid to be rejected")
	}
}

// The dangerous case: a token claims a kid the JWKS never published, signed by
// a DIFFERENT key than the one that IS published. With a keys[0] fallback this
// would be verified against the published key and accepted. It must not be.
func TestVerifyClaims_BogusKID_NoFallbackToFirstKey(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	js := newJWKSServer(t)
	js.publish(&key.PublicKey, "kid-real")

	_, err := newVerifier(js).VerifyClaims(context.Background(),
		signToken(t, key, "kid-does-not-exist", baseClaims("user-1", testIssuer, testAudience)))
	if err == nil {
		t.Fatal("token with unlisted kid must be rejected, not verified against keys[0]")
	}
}

// After the provider rotates its key, a token with the new kid must succeed
// without waiting for the 1h cache TTL to expire.
func TestVerifyClaims_RefetchesJWKSOnKeyRotation(t *testing.T) {
	oldKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	js := newJWKSServer(t)
	js.publish(&oldKey.PublicKey, "kid-old")
	v := newVerifier(js)

	// Warm the cache with the old key set.
	if _, err := v.VerifyClaims(context.Background(), signToken(t, oldKey, "kid-old", baseClaims("user-1", testIssuer, testAudience))); err != nil {
		t.Fatalf("old key should verify: %v", err)
	}
	hitsBefore := js.hits.Load()

	// Provider rotates to a new key.
	newKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	js.publish(&newKey.PublicKey, "kid-new")

	cl, err := v.VerifyClaims(context.Background(), signToken(t, newKey, "kid-new", baseClaims("user-1", testIssuer, testAudience)))
	if err != nil {
		t.Fatalf("expected refetch to pick up rotated key, got %v", err)
	}
	if cl.Sub != "user-1" {
		t.Errorf("expected sub user-1, got %q", cl.Sub)
	}
	if js.hits.Load() <= hitsBefore {
		t.Error("expected a forced JWKS refetch on unknown kid")
	}
}

// A non-200 JWKS response must not be cached, or every request fails for the
// whole TTL after one blip.
func TestFetchJWKS_ErrorResponseNotCached(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	js := newJWKSServer(t)
	js.publish(&key.PublicKey, "kid-1")
	js.status.Store(http.StatusInternalServerError)

	v := newVerifier(js)
	token := signToken(t, key, "kid-1", baseClaims("user-1", testIssuer, testAudience))

	if _, err := v.VerifyClaims(context.Background(), token); err == nil {
		t.Fatal("expected failure while JWKS endpoint is down")
	}

	// Endpoint recovers — the next call must succeed, proving nothing bad was cached.
	js.status.Store(http.StatusOK)
	if _, err := v.VerifyClaims(context.Background(), token); err != nil {
		t.Fatalf("expected recovery after JWKS endpoint returns, got %v", err)
	}
}

func TestVerifyClaims_ES256Token(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	js := newJWKSServer(t)
	js.publishEC(&key.PublicKey, "ec-kid-1")

	cl, err := newVerifier(js).VerifyClaims(context.Background(), signES256Token(t, key, "ec-kid-1", baseClaims("user-1", testIssuer, testAudience)))
	if err != nil {
		t.Fatalf("expected valid ES256 token, got %v", err)
	}
	if cl.Sub != "user-1" {
		t.Errorf("expected sub user-1, got %q", cl.Sub)
	}
}

// A token claiming an RSA kid must be rejected if it's actually signed with
// ES256 — the algorithm is resolved from the kid's own JWK, never trusted
// from the token's header alone.
func TestVerifyClaims_AlgConfusion_RSAKidSignedES256_Rejected(t *testing.T) {
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	js := newJWKSServer(t)
	js.publish(&rsaKey.PublicKey, "shared-kid")

	_, err := newVerifier(js).VerifyClaims(context.Background(),
		signES256Token(t, ecKey, "shared-kid", baseClaims("attacker", testIssuer, testAudience)))
	if err == nil {
		t.Fatal("expected token signed with the wrong algorithm for its kid to be rejected")
	}
}

func TestPing(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	js := newJWKSServer(t)
	js.publish(&key.PublicKey, "kid-1")

	if err := newVerifier(js).Ping(context.Background()); err != nil {
		t.Fatalf("expected Ping to succeed, got %v", err)
	}
}

func TestPing_EmptyKeySetFails(t *testing.T) {
	js := newJWKSServer(t)
	if err := newVerifier(js).Ping(context.Background()); err == nil {
		t.Fatal("expected Ping to fail with no published keys")
	}
}

// missCache is a shared cache that never holds anything: every lookup goes to the
// provider, which is what a Valkey outage looks like to the verifier.
type missCache struct{ cache.Backend }

func (missCache) Get(context.Context, string) ([]byte, bool, error) { return nil, false, nil }
func (missCache) Set(context.Context, string, []byte, int) error    { return nil }

// countingCache counts reads of the shared cache.
type countingCache struct {
	cache.Backend
	gets atomic.Int64
}

func (c *countingCache) Get(ctx context.Context, k string) ([]byte, bool, error) {
	c.gets.Add(1)
	return c.Backend.Get(ctx, k)
}

// Once the keys are parsed, a request touches neither the shared cache nor the
// provider: it used to cost a Valkey GET, a JSON decode and a key rebuild each.
func TestVerifyClaims_ServesParsedKeysFromMemory(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	js := newJWKSServer(t)
	js.publish(&key.PublicKey, "kid-1")
	shared := &countingCache{Backend: cache.NewMemoryBackend(16)}
	v := jwtverify.NewVerifier(js.srv.URL, testAudience, testIssuer, shared)
	token := signToken(t, key, "kid-1", baseClaims("user-1", testIssuer, testAudience))

	if _, err := v.VerifyClaims(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	gets, hits := shared.gets.Load(), js.hits.Load()
	for range 50 {
		if _, err := v.VerifyClaims(context.Background(), token); err != nil {
			t.Fatal(err)
		}
	}
	if got := shared.gets.Load() - gets; got != 0 {
		t.Errorf("%d shared-cache reads over 50 verifications, want 0", got)
	}
	if got := js.hits.Load() - hits; got != 0 {
		t.Errorf("%d provider fetches over 50 verifications, want 0", got)
	}
}

// Concurrent first requests share one load: a cold start or an expiry must not
// become a burst of identical fetches against the identity provider.
func TestVerifyClaims_ColdStartIsOneFetch(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	js := newJWKSServer(t)
	js.publish(&key.PublicKey, "kid-1")
	v := jwtverify.NewVerifier(js.srv.URL, testAudience, testIssuer, missCache{})
	token := signToken(t, key, "kid-1", baseClaims("user-1", testIssuer, testAudience))

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := v.VerifyClaims(context.Background(), token); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := js.hits.Load(); got != 1 {
		t.Fatalf("%d provider fetches for 20 concurrent cold requests, want 1", got)
	}
}

// The provider being down after the keys expired from memory must not fail
// requests signed with a key we still hold; a kid we never held still fails.
func TestVerifyClaims_StaleKeysServeWhileTheProviderIsDown(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	js := newJWKSServer(t)
	js.publish(&key.PublicKey, "kid-1")
	v := jwtverify.NewVerifier(js.srv.URL, testAudience, testIssuer, missCache{}).WithKeyMemoryTTL(time.Nanosecond)
	token := signToken(t, key, "kid-1", baseClaims("user-1", testIssuer, testAudience))

	if _, err := v.VerifyClaims(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	js.status.Store(http.StatusInternalServerError)
	if _, err := v.VerifyClaims(context.Background(), token); err != nil {
		t.Fatalf("a key we hold was refused during a provider outage: %v", err)
	}
	other := signToken(t, key, "kid-never-seen", baseClaims("user-1", testIssuer, testAudience))
	if _, err := v.VerifyClaims(context.Background(), other); err == nil {
		t.Fatal("a kid we never held was accepted during an outage")
	}
}
