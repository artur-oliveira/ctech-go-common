// Package lock provides a CAS (compare-and-swap) acquire/renew/release lock
// keyed by an arbitrary string, backed by Valkey/Redis in production and an
// in-memory store for dev/single-replica/tests (the in-memory store is NOT
// shared across replicas).
//
// It is the shared primitive behind two different usage shapes: a short,
// fire-and-forget advisory lock held for one operation (ctech-wallet's
// per-wallet lock: acquire, do the operation, release) and a long-held lease
// renewed on a heartbeat for a process's entire lifetime handling some
// resource (ctech-poker's per-table lease). TTL is a required constructor
// argument because those two use cases need very different values; per-
// consumer key namespacing (e.g. "wallet:" vs "table:" prefixes) is each
// consumer's own concern, layered on top of this package.
package lock

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/valkey-io/valkey-go"
	"gopkg.aoctech.app/api-commons/cache"
)

// DefaultHeartbeatInterval is a sane default cadence for StartHeartbeat —
// well under most reasonable lease TTLs. Callers with a specific TTL should
// still pass an interval comfortably below it explicitly.
const DefaultHeartbeatInterval = 5 * time.Second

// store is the minimal CAS primitive the Locker needs: atomic acquire,
// owner-checked renew, and owner-checked release.
type store interface {
	setNX(ctx context.Context, key, token string, ttl time.Duration) (bool, error)
	renewIfMatch(ctx context.Context, key, token string, ttl time.Duration) (bool, error)
	delIfMatch(ctx context.Context, key, token string) error
}

// Locker acquires, renews, and releases CAS locks keyed by an arbitrary
// string. Each lock is held for ttl before it auto-expires, so a crashed
// holder can never wedge a key forever (fail safe).
type Locker struct {
	store store
	ttl   time.Duration

	mu     sync.Mutex
	tokens map[string]string // key -> this process's current token, for Renew
}

// New returns a Valkey-backed Locker when the cache backend is Redis,
// otherwise an in-memory one (dev/single-replica only — the in-memory store
// is NOT shared across replicas). ttl bounds how long a lock is held before
// it auto-releases.
func New(c cache.Backend, ttl time.Duration) *Locker {
	l := &Locker{ttl: ttl, tokens: make(map[string]string)}
	if rb, ok := c.(*cache.RedisBackend); ok {
		l.store = &redisStore{client: rb.Client()}
	} else {
		l.store = newMemStore()
	}
	return l
}

// Acquire takes the lock for one key. On success it returns a release func
// (safe to call once) and ok=true. On contention it returns ok=false and no
// error.
func (l *Locker) Acquire(ctx context.Context, key string) (release func(), ok bool, err error) {
	token, err := newToken()
	if err != nil {
		return nil, false, err
	}
	got, err := l.store.setNX(ctx, key, token, l.ttl)
	if err != nil {
		return nil, false, err
	}
	if !got {
		return nil, false, nil
	}
	l.mu.Lock()
	l.tokens[key] = token
	l.mu.Unlock()
	// Release always uses a fresh, non-cancelled context — the caller's ctx
	// may already be done by the time release() runs (e.g. via defer after
	// the request that acquired the lock is cancelled), and a lock must
	// still be freed in that case rather than wedged until its TTL expires.
	return func() {
		_ = l.store.delIfMatch(context.Background(), key, token)
		l.mu.Lock()
		delete(l.tokens, key)
		l.mu.Unlock()
	}, true, nil
}

// AcquireOrdered takes locks for multiple keys in a total order — the keys
// are sorted lexicographically — so any caller locking the same set acquires
// them in the identical order. That deterministic total order is what
// prevents deadlock; it does not depend on what the keys mean. It is
// all-or-nothing: if any lock is contended, the already-held ones are
// released and ok=false is returned.
func (l *Locker) AcquireOrdered(ctx context.Context, keys ...string) (release func(), ok bool, err error) {
	ids := append([]string(nil), keys...)
	sort.Strings(ids)

	releases := make([]func(), 0, len(ids))
	releaseAll := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	for _, id := range ids {
		rel, got, err := l.Acquire(ctx, id)
		if err != nil {
			releaseAll()
			return nil, false, err
		}
		if !got {
			releaseAll()
			return nil, false, nil
		}
		releases = append(releases, rel)
	}
	return releaseAll, true, nil
}

// Renew extends the TTL of a lock this process currently holds. It errors if
// this process no longer holds the key locally (never acquired it, or
// already released it) or if the CAS-renew fails because the lock expired
// and was re-acquired elsewhere — the caller must treat that as "I've lost
// authority over this key" and stop whatever it was protecting immediately.
func (l *Locker) Renew(ctx context.Context, key string) error {
	l.mu.Lock()
	token, held := l.tokens[key]
	l.mu.Unlock()
	if !held {
		return fmt.Errorf("lock: no lock held locally for key %s", key)
	}
	ok, err := l.store.renewIfMatch(ctx, key, token, l.ttl)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("lock: lease for key %s was lost (token mismatch or expired)", key)
	}
	return nil
}

// StartHeartbeat renews the lock for key every interval until the returned
// stop func is called or Renew fails (lease lost) — in which case it calls
// onLost, if provided, and stops. interval is caller-supplied (rather than a
// package-wide constant) so each consumer's own TTL can dictate its own safe
// margin; DefaultHeartbeatInterval is offered as a sane default.
func (l *Locker) StartHeartbeat(ctx context.Context, key string, interval time.Duration, onLost func()) (stop func()) {
	loopCtx, cancel := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-ticker.C:
				if err := l.Renew(loopCtx, key); err != nil {
					if onLost != nil {
						onLost()
					}
					return
				}
			}
		}
	}()
	return cancel
}

func newToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// --- redis-backed store ---

type redisStore struct {
	client valkey.Client
}

func (s *redisStore) setNX(ctx context.Context, key, token string, ttl time.Duration) (bool, error) {
	_, err := s.client.Do(ctx, s.client.B().Set().Key(key).Value(token).Nx().Ex(ttl).Build()).ToString()
	if valkey.IsValkeyNil(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// casRenewScript extends a key's TTL only if its value still matches token,
// so a lock whose TTL already expired (and was re-acquired by someone else)
// is never renewed on the previous owner's behalf.
const casRenewScript = `
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("expire", KEYS[1], ARGV[2])
end
return 0
`

func (s *redisStore) renewIfMatch(ctx context.Context, key, token string, ttl time.Duration) (bool, error) {
	n, err := s.client.Do(ctx, s.client.B().Eval().Script(casRenewScript).Numkeys(1).Key(key).Arg(token, fmt.Sprintf("%d", int(ttl.Seconds()))).Build()).ToInt64()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// casDelScript deletes the key only if its value still matches token, so a
// lock whose TTL already expired (and was re-acquired by someone else) is
// never released by the previous owner.
const casDelScript = `
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("del", KEYS[1])
end
return 0
`

func (s *redisStore) delIfMatch(ctx context.Context, key, token string) error {
	return s.client.Do(ctx, s.client.B().Eval().Script(casDelScript).Numkeys(1).Key(key).Arg(token).Build()).Error()
}

// --- in-memory store (dev/single-replica/tests) ---

type memEntry struct {
	token   string
	expires time.Time
}

type memStore struct {
	mu   sync.Mutex
	keys map[string]memEntry
}

func newMemStore() *memStore { return &memStore{keys: make(map[string]memEntry)} }

func (s *memStore) setNX(_ context.Context, key, token string, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.keys[key]; ok && time.Now().Before(e.expires) {
		return false, nil
	}
	s.keys[key] = memEntry{token: token, expires: time.Now().Add(ttl)}
	return true, nil
}

func (s *memStore) renewIfMatch(_ context.Context, key, token string, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.keys[key]
	if !ok || e.token != token || time.Now().After(e.expires) {
		return false, nil
	}
	s.keys[key] = memEntry{token: token, expires: time.Now().Add(ttl)}
	return true, nil
}

func (s *memStore) delIfMatch(_ context.Context, key, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.keys[key]; ok && e.token == token {
		delete(s.keys, key)
	}
	return nil
}
