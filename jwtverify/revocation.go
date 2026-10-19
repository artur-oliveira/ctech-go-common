package jwtverify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"time"

	"gopkg.aoctech.app/api-commons/cache"
	"gopkg.aoctech.app/api-commons/ttlcache"
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

// revocationMemoryTTL is how long a verifier remembers a user's revocation entry
// (or its absence) without asking Valkey again. It is the delay between ctech-account
// locking a user and this process refusing the user's still-valid access tokens:
// 10 s on top of the near-immediate effect on the refresh path. Money-moving
// routes (VerifyClaimsStrict) skip it and read the list every time.
const revocationMemoryTTL = 10 * time.Second

// revocationCacheMax bounds the per-process memory: one small entry per recently
// seen user.
const revocationCacheMax = 20000

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
	v.revoked = ttlcache.New[revocationEntry](revocationMemoryTTL, revocationCacheMax)
	return v
}

// WithRevocationMemoryTTL overrides how long a revocation answer is remembered.
// Call it after WithRevocation.
func (v *Verifier) WithRevocationMemoryTTL(d time.Duration) *Verifier {
	v.revoked = ttlcache.New[revocationEntry](d, revocationCacheMax)
	return v
}

// revocationEntry is what the list says about one user: not on it, or a cut-off.
type revocationEntry struct {
	revoked bool
	cutoff  int64
}

func (e revocationEntry) verdict(iat int64) error {
	if e.revoked && iat <= e.cutoff {
		return ErrTokenRevoked
	}
	return nil
}

func lookupRevocation(ctx context.Context, c cache.Backend, sub string) (revocationEntry, error) {
	raw, ok, err := c.Get(ctx, revokedSubPrefix+sub)
	if err != nil {
		return revocationEntry{}, fmt.Errorf("%w: %v", ErrRevocationUnavailable, err)
	}
	if !ok {
		return revocationEntry{}, nil
	}
	cutoff, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		// An unparseable entry fails safe: every token of the user is refused.
		return revocationEntry{revoked: true, cutoff: math.MaxInt64}, nil
	}
	return revocationEntry{revoked: true, cutoff: cutoff}, nil
}

// CheckRevoked reports whether sub's token issued at iat is on the revocation
// list. For services that verify tokens without a Verifier (ctech-account signs
// and verifies its own). A backend failure is returned wrapped in
// ErrRevocationUnavailable; the caller chooses to fail open or closed.
func CheckRevoked(ctx context.Context, c cache.Backend, sub string, iat int64) error {
	e, err := lookupRevocation(ctx, c, sub)
	if err != nil {
		return err
	}
	return e.verdict(iat)
}

func (v *Verifier) checkRevoked(ctx context.Context, cl *Claims, strict bool) error {
	if v.revocation == nil {
		return nil
	}
	if !strict {
		if e, ok := v.revoked.Get(cl.Sub); ok {
			return e.verdict(cl.IssuedAt)
		}
	}
	e, err := lookupRevocation(ctx, v.revocation, cl.Sub)
	if err != nil {
		if !strict {
			slog.WarnContext(ctx, "jwtverify: revocation check skipped", "error", err)
			return nil
		}
		return err
	}
	// Only answers are remembered, never an outage: a Valkey blip must not turn
	// into ten seconds of "not revoked" or of failures.
	v.revoked.Set(cl.Sub, e)
	return e.verdict(cl.IssuedAt)
}
