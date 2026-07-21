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
