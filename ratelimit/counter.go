package ratelimit

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/valkey-io/valkey-go"
)

// Counter is the storage a Limiter counts on. Implementations must give Incr
// SET-NX window semantics: the TTL is set when the key is first created and
// never refreshed by later increments.
type Counter interface {
	// Incr atomically increments the counter, creating it with the given
	// window TTL when absent, and returns the post-increment value.
	Incr(ctx context.Context, key string, window time.Duration) (int64, error)
	// Count returns the current value, or 0 when the key is absent/expired.
	Count(ctx context.Context, key string) (int64, error)
}

// TTLCounter is a Counter that can report how long a key has left. Limiter
// uses it for exact Retry-After / RateLimit-Reset values and falls back to the
// full Window when the Counter does not implement it.
type TTLCounter interface {
	Counter
	// TTL returns the remaining life of key, or 0 when it is absent or has no TTL.
	TTL(ctx context.Context, key string) (time.Duration, error)
}

// Store is a TTLCounter that can also drop a key: what Lockout needs
// (reset the failure count on success). ValkeyCounter and MemoryCounter
// implement it.
type Store interface {
	TTLCounter
	Delete(ctx context.Context, key string) error
}

// TTL reads the remaining life of key (PTTL); absent or TTL-less keys give 0.
func (v *ValkeyCounter) TTL(ctx context.Context, key string) (time.Duration, error) {
	ms, err := v.client.Do(ctx, v.client.B().Pttl().Key(key).Build()).AsInt64()
	if err != nil {
		return 0, fmt.Errorf("valkey PTTL: %w", err)
	}
	if ms < 0 {
		return 0, nil
	}
	return time.Duration(ms) * time.Millisecond, nil
}

func (v *ValkeyCounter) Delete(ctx context.Context, key string) error {
	if err := v.client.Do(ctx, v.client.B().Del().Key(key).Build()).Error(); err != nil {
		return fmt.Errorf("valkey DEL: %w", err)
	}
	return nil
}

func (m *MemoryCounter) TTL(_ context.Context, key string) (time.Duration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[key]
	if !ok {
		return 0, nil
	}
	return max(time.Until(e.expires), 0), nil
}

func (m *MemoryCounter) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, key)
	return nil
}

// ValkeyCounter counts on a Valkey/Redis client (cache.RedisBackend.Client()).
type ValkeyCounter struct {
	client valkey.Client
}

func NewValkeyCounter(client valkey.Client) *ValkeyCounter {
	return &ValkeyCounter{client: client}
}

// setNXErr classifies a SET ... NX outcome: valkey.Nil means "key already
// exists" (the normal case), not a transport failure — only genuine errors
// are returned so a FailClosed limiter doesn't 503 every 2nd+ request in a
// window (the CAC-010 regression class).
func setNXErr(err error) error {
	if err == nil || valkey.IsValkeyNil(err) {
		return nil
	}
	return fmt.Errorf("valkey SET NX: %w", err)
}

// Incr seeds the key with SET NX EX (guaranteeing a TTL without clobbering an
// existing counter), then INCRs. The old INCR-then-EXPIRE pair could crash
// between the two ops and leave a TTL-less key — a permanent lockout.
func (v *ValkeyCounter) Incr(ctx context.Context, key string, window time.Duration) (int64, error) {
	setCmd := v.client.B().Set().Key(key).Value("0").Nx().Ex(window).Build()
	if err := setNXErr(v.client.Do(ctx, setCmd).Error()); err != nil {
		return 0, err
	}
	n, err := v.client.Do(ctx, v.client.B().Incr().Key(key).Build()).AsInt64()
	if err != nil {
		return 0, fmt.Errorf("valkey INCR: %w", err)
	}
	return n, nil
}

func (v *ValkeyCounter) Count(ctx context.Context, key string) (int64, error) {
	result := v.client.Do(ctx, v.client.B().Get().Key(key).Build())
	if err := result.Error(); err != nil {
		if valkey.IsValkeyNil(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("valkey GET: %w", err)
	}
	n, err := result.AsInt64()
	if err != nil {
		return 0, fmt.Errorf("reading counter: %w", err)
	}
	return n, nil
}

// MemoryCounter is a process-local Counter for tests and single-replica dev.
// It is NOT shared across replicas — production limiters must use Valkey.
type MemoryCounter struct {
	mu      sync.Mutex
	entries map[string]memEntry
}

type memEntry struct {
	n       int64
	expires time.Time
}

func NewMemoryCounter() *MemoryCounter {
	return &MemoryCounter{entries: map[string]memEntry{}}
}

func (m *MemoryCounter) Incr(_ context.Context, key string, window time.Duration) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[key]
	if !ok || time.Now().After(e.expires) {
		e = memEntry{expires: time.Now().Add(window)} // NX semantics: window starts once
	}
	e.n++
	m.entries[key] = e
	return e.n, nil
}

func (m *MemoryCounter) Count(_ context.Context, key string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[key]
	if !ok || time.Now().After(e.expires) {
		return 0, nil
	}
	return e.n, nil
}
