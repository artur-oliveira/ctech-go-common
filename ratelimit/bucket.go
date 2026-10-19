package ratelimit

import (
	"math"
	"sync"
	"time"
)

// Bucket is an in-process token bucket per key: Rate tokens a second, up to Burst
// held. It is the per-tenant guard that costs nothing per request: no Valkey round
// trip, no shared state. The price is that each process counts alone, so with N
// replicas a key gets up to N times Rate; set Rate for the process, not the fleet.
// A runaway integration of one tenant is stopped well before it starves the others
// on the same instance, which is what it is for. Valkey-backed Limiter is the shape
// for a limit that must hold across replicas (auth, brute force).
//
// Memory is bounded: past MaxKeys the idle keys (buckets that have refilled to
// Burst carry no information) are dropped, and a key that still does not fit is
// allowed rather than counted, so the limiter never becomes the thing that fails.
type Bucket struct {
	rate, burst float64
	maxKeys     int
	now         func() time.Time

	mu      sync.Mutex
	entries map[string]*bucketEntry
}

type bucketEntry struct {
	tokens float64
	at     time.Time
}

// NewBucket returns a Bucket refilling rate tokens a second up to burst, tracking
// at most maxKeys keys (0 means 10000). rate or burst <= 0 returns nil, and a nil
// *Bucket allows everything: the off switch.
func NewBucket(rate float64, burst, maxKeys int) *Bucket {
	if rate <= 0 || burst <= 0 {
		return nil
	}
	if maxKeys <= 0 {
		maxKeys = 10_000
	}
	return &Bucket{rate: rate, burst: float64(burst), maxKeys: maxKeys, now: time.Now, entries: map[string]*bucketEntry{}}
}

// Allow takes one token for key. When it is refused, retryAfter is how long until
// one is available.
func (b *Bucket) Allow(key string) (ok bool, retryAfter time.Duration) {
	if b == nil {
		return true, 0
	}
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[key]
	if e == nil {
		if len(b.entries) >= b.maxKeys {
			b.dropIdle(now)
			if len(b.entries) >= b.maxKeys {
				return true, 0
			}
		}
		e = &bucketEntry{tokens: b.burst, at: now}
		b.entries[key] = e
	}
	e.tokens = math.Min(b.burst, e.tokens+now.Sub(e.at).Seconds()*b.rate)
	e.at = now
	if e.tokens >= 1 {
		e.tokens--
		return true, 0
	}
	return false, time.Duration((1 - e.tokens) / b.rate * float64(time.Second))
}

// dropIdle removes the buckets that would be full again by now. Caller holds mu.
func (b *Bucket) dropIdle(now time.Time) {
	for k, e := range b.entries {
		if math.Min(b.burst, e.tokens+now.Sub(e.at).Seconds()*b.rate) >= b.burst {
			delete(b.entries, k)
		}
	}
}
