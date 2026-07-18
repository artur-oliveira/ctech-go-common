// Package jwtverify validates RS256 access tokens against a JWKS endpoint
// (ctech-account) and parses the claims every consumer service needs. It is
// the shared primitive; per-service authorization policy (RBAC, scope
// families, Fiber locals wiring, error response shape) stays in each service
// — the trust models differ, only the token-validation mechanics don't.
package jwtverify

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"gopkg.aoctech.app/api-commons/cache"
)

const (
	jwksCacheKey = "ctech:jwks"
	jwksTTL      = 3600 // 1 hour

	// minJWKSRefetchInterval throttles forced JWKS refreshes triggered by an
	// unknown kid, so a flood of bogus tokens cannot hammer the identity provider.
	minJWKSRefetchInterval = 60 * time.Second
)

type jwk struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type jwksResponse struct {
	Keys []jwk `json:"keys"`
}

// Claims holds the ctech-account access-token fields its downstream services
// consume. An empty SID marks an M2M client_credentials token.
type Claims struct {
	Sub       string // user_id (or client_id for M2M)
	Scope     string // space-joined scope string
	SID       string // session id; empty for M2M tokens
	AZP       string // OAuth client_id
	KYCLevel  string // "" | "basic" | "verified" (empty when the service/scope doesn't carry it)
	LastMFAAt int64  // unix seconds of the last MFA proof; 0 if absent
}

// Scopes splits the space-joined Scope claim into individual scope strings.
func (c *Claims) Scopes() []string { return strings.Fields(c.Scope) }

// HasScope reports whether the token carries the given scope.
func (c *Claims) HasScope(want string) bool {
	for _, s := range c.Scopes() {
		if s == want {
			return true
		}
	}
	return false
}

// Verifier validates RS256 access tokens issued by ctech-account against its JWKS.
// It bundles the settings every call site needs, so they are configured once.
type Verifier struct {
	jwksURL  string
	audience string // expected aud claim; empty disables the audience check
	issuer   string // expected iss claim; empty disables the issuer check
	cache    cache.Backend

	mu          sync.Mutex
	lastRefetch time.Time
}

func NewVerifier(jwksURL, audience, issuer string, cacheBackend cache.Backend) *Verifier {
	return &Verifier{jwksURL: jwksURL, audience: audience, issuer: issuer, cache: cacheBackend}
}

// Ping reports whether the account JWKS is usable — served from cache when warm,
// fetched otherwise. An empty key set counts as a failure: no token can be
// verified without keys. Used by health checks.
func (v *Verifier) Ping(ctx context.Context) error {
	keys, err := v.fetchJWKS(ctx, false)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return fmt.Errorf("jwks empty")
	}
	return nil
}

// VerifyClaims validates a raw JWT string and returns its parsed claims.
func (v *Verifier) VerifyClaims(ctx context.Context, tokenStr string) (*Claims, error) {
	kid, err := tokenKID(tokenStr)
	if err != nil {
		return nil, err
	}

	pubKey, err := v.keyForKID(ctx, kid)
	if err != nil {
		return nil, err
	}

	var parseOpts []jwt.ParserOption
	if v.audience != "" {
		parseOpts = append(parseOpts, jwt.WithAudience(v.audience))
	}
	if v.issuer != "" {
		parseOpts = append(parseOpts, jwt.WithIssuer(v.issuer))
	}
	token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return pubKey, nil
	}, parseOpts...)
	if err != nil {
		return nil, err
	}

	mc, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid claims")
	}

	cl := &Claims{}
	cl.Sub, _ = mc["sub"].(string)
	cl.Scope, _ = mc["scope"].(string)
	cl.SID, _ = mc["sid"].(string)
	cl.AZP, _ = mc["azp"].(string)
	cl.KYCLevel, _ = mc["kyc_level"].(string)
	if lm, ok := mc["last_mfa_at"].(float64); ok {
		cl.LastMFAAt = int64(lm)
	}
	return cl, nil
}

// keyForKID resolves the signing key for kid. On a cache miss it forces one
// throttled JWKS refresh so a key rotation at the identity provider takes
// effect immediately instead of after the cache TTL. An unresolvable kid is
// rejected — never silently verified against some other key.
func (v *Verifier) keyForKID(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	keys, err := v.fetchJWKS(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("jwks unavailable: %w", err)
	}
	if k := findKID(keys, kid); k != nil {
		return jwkToRSA(k)
	}

	// Unknown kid: the provider may have rotated keys since we cached them.
	keys, err = v.fetchJWKS(ctx, true)
	if err != nil {
		return nil, fmt.Errorf("jwks refresh failed: %w", err)
	}
	if k := findKID(keys, kid); k != nil {
		return jwkToRSA(k)
	}
	return nil, fmt.Errorf("no signing key for kid %q", kid)
}

func findKID(keys []jwk, kid string) *jwk {
	for i := range keys {
		if keys[i].Kid == kid {
			return &keys[i]
		}
	}
	return nil
}

// tokenKID extracts the kid from a JWT header without verifying the signature.
func tokenKID(tokenStr string) (string, error) {
	parts := strings.Split(tokenStr, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("malformed token")
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", err
	}
	var header struct {
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return "", err
	}
	return header.Kid, nil
}

// fetchJWKS returns the provider's signing keys. When force is true the cache is
// bypassed and refreshed, subject to minJWKSRefetchInterval.
func (v *Verifier) fetchJWKS(ctx context.Context, force bool) ([]jwk, error) {
	if !force {
		if data, ok, _ := v.cache.Get(ctx, jwksCacheKey); ok {
			var keys []jwk
			if err := json.Unmarshal(data, &keys); err == nil && len(keys) > 0 {
				return keys, nil
			}
		}
	} else {
		v.mu.Lock()
		if time.Since(v.lastRefetch) < minJWKSRefetchInterval {
			v.mu.Unlock()
			return nil, fmt.Errorf("jwks refresh throttled")
		}
		v.lastRefetch = time.Now()
		v.mu.Unlock()
	}

	keys, err := fetchJWKSFromURL(ctx, v.jwksURL)
	if err != nil {
		return nil, err
	}

	// Only ever cache a usable key set — caching an empty or failed response would
	// break every request for the whole TTL.
	if len(keys) > 0 {
		if data, err := json.Marshal(keys); err == nil {
			_ = v.cache.Set(ctx, jwksCacheKey, data, jwksTTL)
		}
	}
	return keys, nil
}

func fetchJWKSFromURL(ctx context.Context, jwksURL string) ([]jwk, error) {
	httpCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(httpCtx, http.MethodGet, jwksURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func(Body io.ReadCloser) { _ = Body.Close() }(resp.Body)

	// A 4xx/5xx body is not a key set; decoding it would yield zero keys and,
	// before this check, poison the cache for an hour.
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks endpoint returned status %d", resp.StatusCode)
	}

	var jwks jwksResponse
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		return nil, err
	}
	if len(jwks.Keys) == 0 {
		return nil, fmt.Errorf("jwks endpoint returned no keys")
	}
	return jwks.Keys, nil
}

func jwkToRSA(k *jwk) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, fmt.Errorf("jwk: decode N: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, fmt.Errorf("jwk: decode E: %w", err)
	}
	n := new(big.Int).SetBytes(nBytes)
	e := int(new(big.Int).SetBytes(eBytes).Int64())
	return &rsa.PublicKey{N: n, E: e}, nil
}
