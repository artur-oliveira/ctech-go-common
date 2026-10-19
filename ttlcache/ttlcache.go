// Package ttlcache is a small in-process cache with a time-to-live per entry,
// for answers a service re-reads on every request and can afford to hold for a
// few seconds (a revocation check, an account-lock check). It is per process: a
// change made elsewhere is seen when the entry expires, so the TTL is the
// staleness the caller accepts, and a caller that changes the value itself
// calls Delete.
package ttlcache

import (
	"sync"
	"time"
)

type entry[V any] struct {
	v   V
	exp time.Time
}

// Cache is safe for concurrent use.
type Cache[V any] struct {
	ttl time.Duration
	max int
	now func() time.Time

	mu sync.Mutex
	m  map[string]entry[V]
}

// New holds at most max entries, each for ttl.
func New[V any](ttl time.Duration, max int) *Cache[V] {
	return &Cache[V]{ttl: ttl, max: max, now: time.Now, m: make(map[string]entry[V])}
}

// Get returns the live value for key.
func (c *Cache[V]) Get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok || !c.now().Before(e.exp) {
		delete(c.m, key)
		var zero V
		return zero, false
	}
	return e.v, true
}

// Set stores v for key. At capacity it drops the expired entries, and if the
// cache is still full, all of them: shortcut: a flush instead of LRU, fine while
// max is far above the working set of one process, upgrade when it is not.
func (c *Cache[V]) Set(key string, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[key]; !ok && len(c.m) >= c.max {
		now := c.now()
		for k, e := range c.m {
			if !now.Before(e.exp) {
				delete(c.m, k)
			}
		}
		if len(c.m) >= c.max {
			clear(c.m)
		}
	}
	c.m[key] = entry[V]{v: v, exp: c.now().Add(c.ttl)}
}

// Delete forgets key.
func (c *Cache[V]) Delete(key string) {
	c.mu.Lock()
	delete(c.m, key)
	c.mu.Unlock()
}
