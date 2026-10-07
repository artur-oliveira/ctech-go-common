package jwtverify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"gopkg.aoctech.app/api-commons/cache"
)

// Revocation list for account lock/deletion. ctech-account writes an entry when
// it locks a user; every service's Verifier rejects that user's tokens issued
// at or before the cut-off. Entries only need to outlive the access-token TTL:
// refresh tokens are revoked server-side, so no newer token can be minted.
//
// The entries live in Valkey DB 0 (the shared base URL). Services whose main
// cache uses another logical DB must pass a backend built on the base URL.
const revokedSubPrefix = "ctech:jwt:revoked_sub:"

// RevocationTTL is the 15-minute access-token lifetime plus clock skew.
const RevocationTTL = 20 * time.Minute

var (
	ErrTokenRevoked          = errors.New("jwtverify: token revoked")
	ErrRevocationUnavailable = errors.New("jwtverify: revocation list unavailable")
)

// Revoke rejects every token of sub issued at or before cutoff, for ttl.
func Revoke(ctx context.Context, c cache.Backend, sub string, cutoff time.Time, ttl time.Duration) error {
	return c.Set(ctx, revokedSubPrefix+sub, []byte(strconv.FormatInt(cutoff.Unix(), 10)), int(ttl.Seconds()))
}

// Unrevoke removes sub's entry (deletion request cancelled during grace).
func Unrevoke(ctx context.Context, c cache.Backend, sub string) error {
	return c.Delete(ctx, revokedSubPrefix+sub)
}

// WithRevocation enables the revocation check against c and returns v.
func (v *Verifier) WithRevocation(c cache.Backend) *Verifier {
	v.revocation = c
	return v
}

// CheckRevoked reports whether sub's token issued at iat is on the revocation
// list. For services that verify tokens without a Verifier (ctech-account signs
// and verifies its own). A backend failure is returned wrapped in
// ErrRevocationUnavailable; the caller chooses to fail open or closed.
func CheckRevoked(ctx context.Context, c cache.Backend, sub string, iat int64) error {
	raw, ok, err := c.Get(ctx, revokedSubPrefix+sub)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRevocationUnavailable, err)
	}
	if !ok {
		return nil
	}
	cutoff, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || iat <= cutoff {
		return ErrTokenRevoked // an unparseable entry fails safe
	}
	return nil
}

func (v *Verifier) checkRevoked(ctx context.Context, cl *Claims, strict bool) error {
	if v.revocation == nil {
		return nil
	}
	err := CheckRevoked(ctx, v.revocation, cl.Sub, cl.IssuedAt)
	if errors.Is(err, ErrRevocationUnavailable) && !strict {
		slog.WarnContext(ctx, "jwtverify: revocation check skipped", "error", err)
		return nil
	}
	return err
}
