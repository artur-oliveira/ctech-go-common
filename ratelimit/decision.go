package ratelimit

import (
	"context"
	"math"
	"strings"
	"time"
)

// KeySeparator joins the parts of a composite key.
const KeySeparator = ":"

// Key builds a composite limiter identity (IP+route, tenant+user, ...). When
// any part is empty the identity is unresolved and Key returns "": the
// middleware does not limit an empty key, so a missing user id never collapses
// every anonymous caller into one shared bucket.
func Key(parts ...string) string {
	for _, p := range parts {
		if p == "" {
			return ""
		}
	}
	return strings.Join(parts, KeySeparator)
}

// Decision is a Limiter verdict plus what the HTTP headers need.
type Decision struct {
	Outcome   Outcome
	Limit     int64
	Remaining int64
	// Reset is how long until the window ends (the counter's TTL; the full
	// Window when the Counter cannot report it).
	Reset time.Duration
}

// RetryAfterSeconds is Reset rounded up to whole seconds, at least 1, so a
// client is never told to retry before the window has elapsed.
func (d Decision) RetryAfterSeconds() int {
	return max(int(math.Ceil(d.Reset.Seconds())), 1)
}

func (l *Limiter) reset(ctx context.Context, key string) time.Duration {
	if t, ok := l.Counter.(TTLCounter); ok {
		// TTL is advisory: on error the full Window is a safe upper bound.
		if ttl, err := t.TTL(ctx, key); err == nil && ttl > 0 {
			return ttl
		}
	}
	return l.Window
}

func (l *Limiter) decision(ctx context.Context, key string, n int64, deny bool) Decision {
	d := Decision{Outcome: Allow, Limit: l.Max, Remaining: max(l.Max-n, 0), Reset: l.reset(ctx, key)}
	if deny {
		d.Outcome = Deny
	}
	return d
}

// Decide is Take with the data for RateLimit-* / Retry-After headers: it counts
// one event atomically. On a counter error the Outcome follows the fail mode
// and the error is returned for logging.
func (l *Limiter) Decide(ctx context.Context, id string) (Decision, error) {
	key := l.key(id)
	n, err := l.Counter.Incr(ctx, key, l.Window)
	if err != nil {
		return Decision{Outcome: l.onError(), Limit: l.Max, Reset: l.Window}, err
	}
	return l.decision(ctx, key, n, n > l.Max), nil
}

// DecideFailures is CheckFailures with header data: it reads the failure
// budget without spending it.
func (l *Limiter) DecideFailures(ctx context.Context, id string) (Decision, error) {
	key := l.key(id)
	n, err := l.Counter.Count(ctx, key)
	if err != nil {
		return Decision{Outcome: l.onError(), Limit: l.Max, Reset: l.Window}, err
	}
	return l.decision(ctx, key, n, n >= l.Max), nil
}
