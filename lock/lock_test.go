package lock

import (
	"context"
	"testing"
	"time"

	"gopkg.aoctech.app/api-commons/cache"
)

func TestAcquireAndRelease(t *testing.T) {
	l := New(cache.NewMemoryBackend(16), time.Minute)
	ctx := context.Background()

	rel, ok, err := l.Acquire(ctx, "w1")
	if err != nil || !ok {
		t.Fatalf("first acquire: ok=%v err=%v", ok, err)
	}

	// Second acquire on the same key is contended.
	_, ok2, err := l.Acquire(ctx, "w1")
	if err != nil {
		t.Fatalf("second acquire err: %v", err)
	}
	if ok2 {
		t.Fatal("expected contention on held key")
	}

	// Different key is independent.
	rel3, ok3, _ := l.Acquire(ctx, "w2")
	if !ok3 {
		t.Fatal("expected independent key to acquire")
	}
	rel3()

	// After release, the key is acquirable again.
	rel()
	_, ok4, _ := l.Acquire(ctx, "w1")
	if !ok4 {
		t.Fatal("expected reacquire after release")
	}
}

func TestAcquireOrderedAllOrNothing(t *testing.T) {
	l := New(cache.NewMemoryBackend(16), time.Minute)
	ctx := context.Background()

	// Pre-hold one key so the ordered acquire must fail cleanly.
	rel, ok, _ := l.Acquire(ctx, "sandbox")
	if !ok {
		t.Fatal("setup acquire failed")
	}

	_, ok2, err := l.AcquireOrdered(ctx, "real", "sandbox")
	if err != nil {
		t.Fatalf("ordered acquire err: %v", err)
	}
	if ok2 {
		t.Fatal("expected ordered acquire to fail when one lock is held")
	}

	// The "real" lock must have been released back (all-or-nothing).
	relReal, okReal, _ := l.Acquire(ctx, "real")
	if !okReal {
		t.Fatal("expected 'real' to be free after failed ordered acquire")
	}
	relReal()
	rel()

	// Now both free -> ordered acquire succeeds.
	relAll, ok3, _ := l.AcquireOrdered(ctx, "real", "sandbox")
	if !ok3 {
		t.Fatal("expected ordered acquire to succeed when both free")
	}
	relAll()
}

func TestAcquireThenContendedAcquireFails(t *testing.T) {
	l := New(cache.NewMemoryBackend(100), 15*time.Second)
	ctx := context.Background()

	release, ok, err := l.Acquire(ctx, "table-1")
	if err != nil || !ok {
		t.Fatalf("expected first acquire to succeed, got ok=%v err=%v", ok, err)
	}
	defer release()

	_, ok2, err2 := l.Acquire(ctx, "table-1")
	if err2 != nil {
		t.Fatalf("unexpected error: %v", err2)
	}
	if ok2 {
		t.Fatal("expected contended acquire to fail while lease is held")
	}
}

func TestReleaseFreesLeaseForNewAcquire(t *testing.T) {
	l := New(cache.NewMemoryBackend(100), 15*time.Second)
	ctx := context.Background()

	release, ok, _ := l.Acquire(ctx, "table-2")
	if !ok {
		t.Fatal("expected first acquire to succeed")
	}
	release()

	_, ok2, err2 := l.Acquire(ctx, "table-2")
	if err2 != nil || !ok2 {
		t.Fatalf("expected acquire after release to succeed, got ok=%v err=%v", ok2, err2)
	}
}

func TestRenewExtendsLeaseBeforeExpiry(t *testing.T) {
	l := New(cache.NewMemoryBackend(100), 50*time.Millisecond)
	ctx := context.Background()

	_, ok, _ := l.Acquire(ctx, "table-3")
	if !ok {
		t.Fatal("expected acquire to succeed")
	}

	time.Sleep(30 * time.Millisecond)
	if err := l.Renew(ctx, "table-3"); err != nil {
		t.Fatalf("renew failed: %v", err)
	}

	time.Sleep(30 * time.Millisecond) // 60ms since acquire, but only 30ms since renew
	_, ok2, _ := l.Acquire(ctx, "table-3")
	if ok2 {
		t.Fatal("expected contended acquire to still fail — renew should have extended the lease")
	}
}

func TestLeaseExpiresAndCanBeReacquiredWithoutRenew(t *testing.T) {
	l := New(cache.NewMemoryBackend(100), 20*time.Millisecond)
	ctx := context.Background()

	_, ok, _ := l.Acquire(ctx, "table-4")
	if !ok {
		t.Fatal("expected acquire to succeed")
	}

	time.Sleep(40 * time.Millisecond) // let it expire, never renewed

	_, ok2, err2 := l.Acquire(ctx, "table-4")
	if err2 != nil || !ok2 {
		t.Fatalf("expected acquire after expiry to succeed, got ok=%v err=%v", ok2, err2)
	}
}

func TestRenewFailsWhenNotHeldLocally(t *testing.T) {
	l := New(cache.NewMemoryBackend(100), time.Minute)
	ctx := context.Background()

	if err := l.Renew(ctx, "never-acquired"); err == nil {
		t.Fatal("expected renew to fail for a key this Locker never acquired")
	}
}

func TestStartHeartbeatKeepsLeaseAliveUntilStopped(t *testing.T) {
	l := New(cache.NewMemoryBackend(100), 60*time.Millisecond)
	ctx := context.Background()

	_, ok, _ := l.Acquire(ctx, "table-5")
	if !ok {
		t.Fatal("expected acquire to succeed")
	}

	stop := l.StartHeartbeat(ctx, "table-5", 20*time.Millisecond, nil)
	time.Sleep(100 * time.Millisecond) // longer than the 60ms TTL without heartbeat renewal

	_, ok2, _ := l.Acquire(ctx, "table-5")
	if ok2 {
		t.Fatal("expected lease to still be held — heartbeat should have kept renewing it")
	}
	stop()

	time.Sleep(100 * time.Millisecond) // long enough for the now-unrenewed lease to expire
	_, ok3, _ := l.Acquire(ctx, "table-5")
	if !ok3 {
		t.Fatal("expected lease to expire and become acquirable after heartbeat stopped")
	}
}

func TestStartHeartbeatCallsOnLostWhenLeaseExpiresBeforeRenewal(t *testing.T) {
	l := New(cache.NewMemoryBackend(100), 20*time.Millisecond)
	ctx := context.Background()

	_, ok, _ := l.Acquire(ctx, "table-6")
	if !ok {
		t.Fatal("expected acquire to succeed")
	}

	lost := make(chan struct{})
	// Heartbeat interval deliberately longer than the TTL, so the lease
	// expires before the first renewal fires — simulating a renew that
	// arrives too late (e.g. a slow/blocked process) and confirming onLost
	// fires without racing a second acquirer.
	stop := l.StartHeartbeat(ctx, "table-6", 50*time.Millisecond, func() { close(lost) })
	defer stop()

	select {
	case <-lost:
	case <-time.After(1 * time.Second):
		t.Fatal("expected onLost to be called once the lease expired before the first renewal")
	}
}
