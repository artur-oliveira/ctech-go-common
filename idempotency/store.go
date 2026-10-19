// Package idempotency makes mutating HTTP routes safe to retry (Fiber v3).
// See README.md for the contract, the table schema and the fail-open/closed
// decisions.
package idempotency

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Defaults for Config and the stores.
const (
	// DefaultTTL is how long a recorded answer is replayable.
	DefaultTTL = 24 * time.Hour
	// DefaultLease is how long a claim holds without an answer: past it, a
	// process that died mid-request no longer blocks the key. Keep it longer
	// than the slowest request (the per-request deadline) and short enough that
	// a crashed request does not block its retry for long.
	DefaultLease = 2 * time.Minute
)

var (
	// ErrConflict: the key was already used with a different request.
	ErrConflict = errors.New("idempotency key reused with a different request")
	// ErrInProgress: the first request with this key has not answered yet.
	ErrInProgress = errors.New("a request with this idempotency key is still running")
)

// Record is a request's answer, as stored and replayed.
type Record struct {
	RequestHash string
	Status      int
	ContentType string
	Body        []byte
}

// Store persists claims and answers. scope is the tenant namespace chosen by
// the Config.Scope function; the same key in two scopes is two requests.
//
// Implementations must make Claim atomic across processes.
type Store interface {
	// Claim reserves (scope, key) for a request about to run, for lease. It
	// returns (nil, nil) when the caller now holds the claim; the stored Record
	// when the key already completed with the same hash (a replay);
	// ErrConflict when the stored hash differs; ErrInProgress while another
	// request holds a live claim. An entry past its expiry counts as free,
	// even if the backend has not physically deleted it yet.
	Claim(ctx context.Context, scope, key, requestHash string, lease time.Duration) (*Record, error)
	// Complete turns the caller's claim into a replayable answer kept for ttl.
	Complete(ctx context.Context, scope, key string, rec Record, ttl time.Duration) error
	// Release gives up a claim (never removes an answer) so a retry can run.
	Release(ctx context.Context, scope, key string) error
}

// MemoryStore is a single-process Store for tests and local development. It is
// NOT safe behind a load balancer: each process would hold its own claims.
type MemoryStore struct {
	// Now is the clock; time.Now when nil.
	Now func() time.Time

	mu sync.Mutex
	m  map[string]memEntry
}

type memEntry struct {
	rec     Record
	done    bool
	expires time.Time
}

func (s *MemoryStore) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func memKey(scope, key string) string { return scope + "\x00" + key }

func (s *MemoryStore) Claim(_ context.Context, scope, key, requestHash string, lease time.Duration) (*Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	k := memKey(scope, key)
	if e, ok := s.m[k]; ok && now.Before(e.expires) {
		switch {
		case e.rec.RequestHash != requestHash:
			return nil, ErrConflict
		case !e.done:
			return nil, ErrInProgress
		}
		rec := e.rec
		return &rec, nil
	}
	if s.m == nil {
		s.m = map[string]memEntry{}
	}
	s.m[k] = memEntry{rec: Record{RequestHash: requestHash}, expires: now.Add(lease)}
	return nil, nil
}

func (s *MemoryStore) Complete(_ context.Context, scope, key string, rec Record, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]memEntry{}
	}
	s.m[memKey(scope, key)] = memEntry{rec: rec, done: true, expires: s.now().Add(ttl)}
	return nil
}

func (s *MemoryStore) Release(_ context.Context, scope, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := memKey(scope, key)
	if e, ok := s.m[k]; ok && !e.done {
		delete(s.m, k)
	}
	return nil
}
