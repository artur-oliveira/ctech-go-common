// Package ratelimit is the shared, transport-agnostic core of the ecosystem's
// Valkey-backed rate limiting (cross-project rule D15; extracted from
// ctech-account's middleware, B19). It deliberately contains no HTTP
// framework types — api-commons carries no Fiber dependency — so each API
// wraps a Limiter in a thin middleware that maps Deny/Unavailable to its own
// RFC 7807 responses (see problem.TooManyRequests).
//
// Two usage shapes, mirroring the account middleware:
//
//	Throughput guard (every request counts):
//	    switch out, _ := lim.Take(ctx, userID); out { ... }
//
//	Brute-force guard (only failures count):
//	    pre-check with CheckFailures before the handler; after a 4xx/5xx
//	    response call RecordFailure.
package ratelimit

import (
	"context"
	"time"
)

// KeyPrefix namespaces every limiter key in the shared cache.
const KeyPrefix = "rl:"

// Outcome is a limiting decision.
type Outcome int

const (
	// Allow lets the request through.
	Allow Outcome = iota
	// Deny means the caller exhausted its budget → 429.
	Deny
	// Unavailable means a FailClosed limiter could not enforce (counter
	// error) → 503. A missing limiter must degrade to denial, not to
	// unbounded brute force (SEC-002/SEC-003).
	Unavailable
)

// Limiter enforces Max counted events per Window per identity.
type Limiter struct {
	Counter Counter
	// Prefix namespaces the counter (e.g. "login", "user") so limiters don't collide.
	Prefix string
	Max    int64
	Window time.Duration
	// FailClosed turns counter errors into Unavailable instead of Allow —
	// the correct posture for auth surfaces.
	FailClosed bool
}

func (l *Limiter) key(id string) string {
	return KeyPrefix + l.Prefix + ":" + id
}

// Take atomically counts one event and decides — the throughput-guard shape
// (no read-then-write TOCTOU). The returned error is the underlying counter
// failure, for logging; the Outcome already accounts for FailClosed.
func (l *Limiter) Take(ctx context.Context, id string) (Outcome, error) {
	n, err := l.Counter.Incr(ctx, l.key(id), l.Window)
	if err != nil {
		if l.FailClosed {
			return Unavailable, err
		}
		return Allow, err
	}
	if n > l.Max {
		return Deny, nil
	}
	return Allow, nil
}

// CheckFailures reads the failure budget without spending it — the
// brute-force-guard pre-check. Callers count actual failures afterwards with
// RecordFailure.
func (l *Limiter) CheckFailures(ctx context.Context, id string) (Outcome, error) {
	n, err := l.Counter.Count(ctx, l.key(id))
	if err != nil {
		if l.FailClosed {
			return Unavailable, err
		}
		return Allow, err
	}
	if n >= l.Max {
		return Deny, nil
	}
	return Allow, nil
}

// RecordFailure spends one unit of the failure budget (call after a 4xx/5xx
// that involved a guessable credential). Errors are returned for logging
// only — a failed record never blocks the already-sent response.
func (l *Limiter) RecordFailure(ctx context.Context, id string) error {
	_, err := l.Counter.Incr(ctx, l.key(id), l.Window)
	return err
}
