package ratelimit

import (
	"context"
	"time"
)

// Key segments of a Lockout, under KeyPrefix.
const (
	lockoutFailuresSegment = "lockout:fail:"
	lockoutBlockSegment    = "lockout:block:"
)

// Lockout is a generic "N failures, then blocked for a while" guard (account
// lockout after bad passwords, OTP guesses, ...). Failures are counted in a
// fixed Window; the Threshold-th failure sets a block that lasts BlockFor and
// is not extended by further failures. A success calls Reset.
//
// Typical flow: Check before the credential test, Fail on a credential error,
// Reset on success. To avoid an enumeration oracle, answer a blocked identity
// exactly like a wrong credential.
type Lockout struct {
	Store     Store
	Prefix    string
	Threshold int64
	// Window is the failure-counting period.
	Window time.Duration
	// BlockFor is how long the block lasts once Threshold is reached.
	BlockFor time.Duration
	// FailMode applies to Check when the store errors; the zero value is
	// FailOpen. Auth surfaces should use FailClosed.
	FailMode FailMode
}

func (l *Lockout) failKey(id string) string {
	return KeyPrefix + lockoutFailuresSegment + l.Prefix + ":" + id
}
func (l *Lockout) blockKey(id string) string {
	return KeyPrefix + lockoutBlockSegment + l.Prefix + ":" + id
}

// Check reports whether id is blocked: Deny with the remaining block time as
// retryAfter, Allow otherwise, Unavailable when the store errors and the mode
// is FailClosed (the error is returned for logging in every case).
func (l *Lockout) Check(ctx context.Context, id string) (out Outcome, retryAfter time.Duration, err error) {
	n, err := l.Store.Count(ctx, l.blockKey(id))
	if err != nil {
		if l.FailMode == FailClosed {
			return Unavailable, 0, err
		}
		return Allow, 0, err
	}
	if n == 0 {
		return Allow, 0, nil
	}
	ttl, terr := l.Store.TTL(ctx, l.blockKey(id))
	if terr != nil || ttl <= 0 {
		ttl = l.BlockFor // advisory hint only; the block itself is certain
	}
	return Deny, ttl, nil
}

// Fail records one failure and reports whether the count has reached the
// threshold (the block is in force). Errors are for logging only: a failed
// record never blocks the response.
func (l *Lockout) Fail(ctx context.Context, id string) (blocked bool, err error) {
	n, err := l.Store.Incr(ctx, l.failKey(id), l.Window)
	if err != nil || n < l.Threshold {
		return false, err
	}
	// Incr seeds the TTL once (NX), so a block already in force is not extended.
	if _, err := l.Store.Incr(ctx, l.blockKey(id), l.BlockFor); err != nil {
		return false, err
	}
	return true, nil
}

// Reset clears the failure count (call on success). An existing block is kept:
// a correct guess must not lift a lockout.
func (l *Lockout) Reset(ctx context.Context, id string) error {
	return l.Store.Delete(ctx, l.failKey(id))
}

// Unblock lifts the block and the count (admin unlock, password reset flow).
func (l *Lockout) Unblock(ctx context.Context, id string) error {
	if err := l.Store.Delete(ctx, l.blockKey(id)); err != nil {
		return err
	}
	return l.Store.Delete(ctx, l.failKey(id))
}
