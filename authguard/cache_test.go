package authguard

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCacheSingleflightAndHit(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	c := NewCache(func(_ context.Context, key string) (string, error) {
		calls.Add(1)
		<-release
		return "v:" + key, nil
	}, CacheOptions{})

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if v, err := c.Get(context.Background(), "k"); err != nil || v != "v:k" {
				t.Errorf("got %q, %v", v, err)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if _, err := c.Get(context.Background(), "k"); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("loader calls = %d, want 1", n)
	}
}

func TestCacheErrorsNotCached(t *testing.T) {
	var calls atomic.Int32
	boom := errors.New("boom")
	c := NewCache(func(context.Context, string) (int, error) {
		if calls.Add(1) == 1 {
			return 0, boom
		}
		return 7, nil
	}, CacheOptions{})
	if _, err := c.Get(context.Background(), "k"); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if v, err := c.Get(context.Background(), "k"); err != nil || v != 7 {
		t.Fatalf("got %d, %v", v, err)
	}
}

func TestCacheLoaderPanicBecomesError(t *testing.T) {
	c := NewCache(func(context.Context, string) (int, error) { panic("x") }, CacheOptions{})
	if _, err := c.Get(context.Background(), "k"); err == nil {
		t.Fatal("want error")
	}
}

func TestCacheCallerCancelDoesNotFailLoad(t *testing.T) {
	release := make(chan struct{})
	c := NewCache(func(context.Context, string) (int, error) { <-release; return 1, nil }, CacheOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Get(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	close(release)
	time.Sleep(50 * time.Millisecond)
	if v, err := c.Get(context.Background(), "k"); err != nil || v != 1 {
		t.Fatalf("got %d, %v (load should have completed and been cached)", v, err)
	}
}

func TestCacheLoadTimeout(t *testing.T) {
	c := NewCache(func(ctx context.Context, _ string) (int, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	}, CacheOptions{LoadTimeout: 20 * time.Millisecond})
	if _, err := c.Get(context.Background(), "k"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestCacheInvalidate(t *testing.T) {
	var n atomic.Int32
	c := NewCache(func(context.Context, string) (int32, error) { return n.Add(1), nil }, CacheOptions{})
	var hooked []string
	c.OnInvalidate(func(k string) { hooked = append(hooked, k) })

	v1, _ := c.Get(context.Background(), "k")
	c.Invalidate("k")
	v2, _ := c.Get(context.Background(), "k")
	if v1 == v2 {
		t.Fatalf("Invalidate did not force a reload: %d == %d", v1, v2)
	}
	c.Forget("k")
	if v3, _ := c.Get(context.Background(), "k"); v3 == v2 {
		t.Fatal("Forget did not force a reload")
	}
	if len(hooked) != 1 || hooked[0] != "k" {
		t.Fatalf("hooks = %v, want exactly one call from Invalidate (not Forget)", hooked)
	}
}

func TestCacheInvalidateDuringLoadDropsStaleResult(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var n atomic.Int32
	c := NewCache(func(context.Context, string) (int32, error) {
		if n.Add(1) == 1 {
			close(started)
			<-release
			return 1, nil // stale: read before the change
		}
		return 2, nil
	}, CacheOptions{})

	done := make(chan int32)
	go func() { v, _ := c.Get(context.Background(), "k"); done <- v }()
	<-started
	c.Invalidate("k")
	close(release)
	<-done
	time.Sleep(50 * time.Millisecond)
	if v, _ := c.Get(context.Background(), "k"); v != 2 {
		t.Fatalf("got %d, want fresh 2 (stale in-flight result must not be cached)", v)
	}
}

func TestCacheTTLExpiry(t *testing.T) {
	var n atomic.Int32
	c := NewCache(func(context.Context, string) (int32, error) { return n.Add(1), nil }, CacheOptions{TTL: 20 * time.Millisecond})
	c.Get(context.Background(), "k")
	time.Sleep(40 * time.Millisecond)
	if v, _ := c.Get(context.Background(), "k"); v != 2 {
		t.Fatalf("got %d, want reload after TTL", v)
	}
}

func TestNewCacheNilLoaderPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("want panic")
		}
	}()
	NewCache[int](nil, CacheOptions{})
}
