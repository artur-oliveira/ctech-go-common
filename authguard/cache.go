package authguard

import (
	"context"
	"fmt"
	"sync"
	"time"

	"gopkg.aoctech.app/api-commons/ttlcache"
)

// Defaults for NewCache.
const (
	DefaultCacheTTL    = 30 * time.Second
	DefaultCacheMax    = 10000
	DefaultLoadTimeout = 2 * time.Second
)

const (
	errLoaderPanicFmt   = "authguard: loader panicked: %v"
	errLoaderMissingMsg = "authguard: NewCache requires a loader"
)

// CacheOptions tunes a Cache. Zero values take the Default* constants.
type CacheOptions struct {
	// TTL is the staleness the caller accepts: a change made elsewhere is seen
	// at most TTL later unless Invalidate/Forget is called.
	TTL time.Duration
	// MaxEntries bounds memory (ttlcache flushes when full).
	MaxEntries int
	// LoadTimeout bounds one loader call. The load runs detached from any single
	// request's context so one cancelled caller does not fail the others
	// waiting on the same key.
	LoadTimeout time.Duration
}

type call[V any] struct {
	done chan struct{}
	v    V
	err  error
}

// Cache is an in-process read-through cache for auth data that is re-read on
// every request (an M2M credential, an organization, an account block). Concurrent
// misses for one key share a single loader call (singleflight). Errors are never
// cached: the next Get retries, so an outage is not prolonged by the cache.
// Absence is the loader's business: return a zero/"absent" V, not an error, to cache it.
//
// It is per process. Other instances see a change when the TTL expires, or
// when the product forwards Invalidate through a hook (OnInvalidate) and
// calls Forget on receipt.
type Cache[V any] struct {
	load    func(ctx context.Context, key string) (V, error)
	timeout time.Duration
	tc      *ttlcache.Cache[V]

	mu    sync.Mutex
	calls map[string]*call[V]
	gen   uint64 // bumped by every invalidation; a load that started before it must not be stored
	hooks []func(key string)
}

// NewCache builds a Cache that fills misses with load. It panics on a nil loader (programmer error).
func NewCache[V any](load func(ctx context.Context, key string) (V, error), opts CacheOptions) *Cache[V] {
	if load == nil {
		panic(errLoaderMissingMsg)
	}
	if opts.TTL <= 0 {
		opts.TTL = DefaultCacheTTL
	}
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = DefaultCacheMax
	}
	if opts.LoadTimeout <= 0 {
		opts.LoadTimeout = DefaultLoadTimeout
	}
	return &Cache[V]{
		load:    load,
		timeout: opts.LoadTimeout,
		tc:      ttlcache.New[V](opts.TTL, opts.MaxEntries),
		calls:   make(map[string]*call[V]),
	}
}

// Get returns the cached value for key, or loads it once for all concurrent callers.
func (c *Cache[V]) Get(ctx context.Context, key string) (V, error) {
	if v, ok := c.tc.Get(key); ok {
		return v, nil
	}
	c.mu.Lock()
	cl, ok := c.calls[key]
	if !ok {
		cl = &call[V]{done: make(chan struct{})}
		c.calls[key] = cl
		go c.run(key, cl, c.gen)
	}
	c.mu.Unlock()

	select {
	case <-cl.done:
		return cl.v, cl.err
	case <-ctx.Done():
		var zero V
		return zero, ctx.Err()
	}
}

func (c *Cache[V]) run(key string, cl *call[V], startGen uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	func() {
		defer func() {
			if r := recover(); r != nil {
				cl.err = fmt.Errorf(errLoaderPanicFmt, r)
			}
		}()
		cl.v, cl.err = c.load(ctx, key)
	}()

	c.mu.Lock()
	if c.calls[key] == cl {
		delete(c.calls, key)
	}
	if cl.err == nil && c.gen == startGen {
		c.tc.Set(key, cl.v)
	}
	c.mu.Unlock()
	close(cl.done)
}

// OnInvalidate registers fn to run after every Invalidate (not Forget), so a
// product can broadcast the key to its other instances. Register before serving.
func (c *Cache[V]) OnInvalidate(fn func(key string)) {
	c.mu.Lock()
	c.hooks = append(c.hooks, fn)
	c.mu.Unlock()
}

// Forget drops key locally, without running hooks. Call it when another
// instance announced the change (calling Invalidate there would echo it back).
func (c *Cache[V]) Forget(key string) {
	c.mu.Lock()
	c.gen++
	delete(c.calls, key) // later Gets start a fresh load instead of joining a stale one
	c.tc.Delete(key)
	c.mu.Unlock()
}

// Invalidate drops key locally, then runs the OnInvalidate hooks. Call it
// right after the product itself changes the underlying data.
func (c *Cache[V]) Invalidate(key string) {
	c.Forget(key)
	c.mu.Lock()
	hooks := append([]func(string){}, c.hooks...)
	c.mu.Unlock()
	for _, h := range hooks {
		h(key)
	}
}
