package jwtverify_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"testing"
	"time"

	"gopkg.aoctech.app/api-commons/cache"
	"gopkg.aoctech.app/api-commons/jwtverify"
)

type downBackend struct{}

var errDown = errors.New("valkey down")

func (downBackend) Get(context.Context, string) ([]byte, bool, error) { return nil, false, errDown }
func (downBackend) Set(context.Context, string, []byte, int) error    { return errDown }
func (downBackend) Delete(context.Context, string) error              { return errDown }
func (downBackend) DeletePrefix(context.Context, string) error        { return errDown }
func (downBackend) Ping(context.Context) error                        { return errDown }

func revocationFixture(t *testing.T) (*rsa.PrivateKey, *jwtverify.Verifier, cache.Backend) {
	t.Helper()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	js := newJWKSServer(t)
	js.publish(&key.PublicKey, "kid-1")
	rev := cache.NewMemoryBackend(16)
	return key, newVerifier(js).WithRevocation(rev), rev
}

func tokenIssuedAt(t *testing.T, key *rsa.PrivateKey, sub string, iat time.Time) string {
	t.Helper()
	claims := baseClaims(sub, testIssuer, testAudience)
	claims["iat"] = iat.Unix()
	return signToken(t, key, "kid-1", claims)
}

func TestVerifyClaims_ExtractsIssuedAt(t *testing.T) {
	key, v, _ := revocationFixture(t)
	iat := time.Now().Add(-time.Minute)
	cl, err := v.VerifyClaims(context.Background(), tokenIssuedAt(t, key, "user-1", iat))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cl.IssuedAt != iat.Unix() {
		t.Errorf("IssuedAt = %d, want %d", cl.IssuedAt, iat.Unix())
	}
}

func TestRevocation_TokenIssuedBeforeCutoffRejected(t *testing.T) {
	ctx := context.Background()
	key, v, rev := revocationFixture(t)
	tok := tokenIssuedAt(t, key, "user-1", time.Now().Add(-time.Minute))
	if err := jwtverify.Revoke(ctx, rev, "user-1", time.Now(), jwtverify.RevocationTTL); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := v.VerifyClaims(ctx, tok); !errors.Is(err, jwtverify.ErrTokenRevoked) {
		t.Fatalf("VerifyClaims err = %v, want ErrTokenRevoked", err)
	}
	if _, err := v.VerifyClaimsStrict(ctx, tok); !errors.Is(err, jwtverify.ErrTokenRevoked) {
		t.Fatalf("VerifyClaimsStrict err = %v, want ErrTokenRevoked", err)
	}
}

func TestRevocation_TokenIssuedAtCutoffSecondRejected(t *testing.T) {
	ctx := context.Background()
	key, v, rev := revocationFixture(t)
	now := time.Now()
	_ = jwtverify.Revoke(ctx, rev, "user-1", now, jwtverify.RevocationTTL)
	if _, err := v.VerifyClaims(ctx, tokenIssuedAt(t, key, "user-1", now)); !errors.Is(err, jwtverify.ErrTokenRevoked) {
		t.Fatalf("err = %v, want ErrTokenRevoked for iat == cutoff", err)
	}
}

func TestRevocation_TokenIssuedAfterCutoffAccepted(t *testing.T) {
	ctx := context.Background()
	key, v, rev := revocationFixture(t)
	_ = jwtverify.Revoke(ctx, rev, "user-1", time.Now().Add(-time.Hour), jwtverify.RevocationTTL)
	if _, err := v.VerifyClaims(ctx, tokenIssuedAt(t, key, "user-1", time.Now())); err != nil {
		t.Fatalf("token issued after cutoff rejected: %v", err)
	}
}

func TestRevocation_OtherSubUnaffected(t *testing.T) {
	ctx := context.Background()
	key, v, rev := revocationFixture(t)
	_ = jwtverify.Revoke(ctx, rev, "user-1", time.Now(), jwtverify.RevocationTTL)
	if _, err := v.VerifyClaims(ctx, tokenIssuedAt(t, key, "user-2", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("other sub rejected: %v", err)
	}
}

func TestRevocation_UnrevokeRestoresAccess(t *testing.T) {
	ctx := context.Background()
	key, v, rev := revocationFixture(t)
	tok := tokenIssuedAt(t, key, "user-1", time.Now().Add(-time.Minute))
	_ = jwtverify.Revoke(ctx, rev, "user-1", time.Now(), jwtverify.RevocationTTL)
	if err := jwtverify.Unrevoke(ctx, rev, "user-1"); err != nil {
		t.Fatalf("Unrevoke: %v", err)
	}
	if _, err := v.VerifyClaims(ctx, tok); err != nil {
		t.Fatalf("token rejected after Unrevoke: %v", err)
	}
}

func TestRevocation_TokenWithoutIatRejectedWhenRevoked(t *testing.T) {
	ctx := context.Background()
	key, v, rev := revocationFixture(t)
	claims := baseClaims("user-1", testIssuer, testAudience)
	delete(claims, "iat")
	_ = jwtverify.Revoke(ctx, rev, "user-1", time.Now(), jwtverify.RevocationTTL)
	if _, err := v.VerifyClaims(ctx, signToken(t, key, "kid-1", claims)); !errors.Is(err, jwtverify.ErrTokenRevoked) {
		t.Fatalf("err = %v, want ErrTokenRevoked", err)
	}
}

func TestRevocation_BackendDown(t *testing.T) {
	ctx := context.Background()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	js := newJWKSServer(t)
	js.publish(&key.PublicKey, "kid-1")
	v := newVerifier(js).WithRevocation(downBackend{})
	tok := tokenIssuedAt(t, key, "user-1", time.Now())

	if _, err := v.VerifyClaims(ctx, tok); err != nil {
		t.Fatalf("VerifyClaims must fail open, got %v", err)
	}
	if _, err := v.VerifyClaimsStrict(ctx, tok); !errors.Is(err, jwtverify.ErrRevocationUnavailable) {
		t.Fatalf("VerifyClaimsStrict err = %v, want ErrRevocationUnavailable", err)
	}
}

func TestRevocation_NotConfiguredSkipsCheck(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	js := newJWKSServer(t)
	js.publish(&key.PublicKey, "kid-1")
	if _, err := newVerifier(js).VerifyClaimsStrict(context.Background(), tokenIssuedAt(t, key, "user-1", time.Now())); err != nil {
		t.Fatalf("strict verify without revocation backend: %v", err)
	}
}
