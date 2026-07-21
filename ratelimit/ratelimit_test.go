package ratelimit

import (
	"context"
	"errors"
	"testing"
	"time"
)

type failingCounter struct{}

func (failingCounter) Incr(context.Context, string, time.Duration) (int64, error) {
	return 0, errors.New("valkey down")
}
func (failingCounter) Count(context.Context, string) (int64, error) {
	return 0, errors.New("valkey down")
}

func TestTake_DeniesAboveMax(t *testing.T) {
	lim := &Limiter{Counter: NewMemoryCounter(), Prefix: "user", Max: 2, Window: time.Minute}
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if out, err := lim.Take(ctx, "u1"); out != Allow || err != nil {
			t.Fatalf("request %d: got %v (%v), want Allow", i+1, out, err)
		}
	}
	if out, _ := lim.Take(ctx, "u1"); out != Deny {
		t.Fatalf("3rd request: got %v, want Deny", out)
	}
	// Other identities have their own budget.
	if out, _ := lim.Take(ctx, "u2"); out != Allow {
		t.Fatalf("other id: got %v, want Allow", out)
	}
}

func TestFailureBudget(t *testing.T) {
	lim := &Limiter{Counter: NewMemoryCounter(), Prefix: "login", Max: 2, Window: time.Minute}
	ctx := context.Background()

	if out, _ := lim.CheckFailures(ctx, "ip1"); out != Allow {
		t.Fatal("fresh identity must be allowed")
	}
	_ = lim.RecordFailure(ctx, "ip1")
	_ = lim.RecordFailure(ctx, "ip1")
	if out, _ := lim.CheckFailures(ctx, "ip1"); out != Deny {
		t.Fatal("exhausted failure budget must deny")
	}
}

func TestFailClosed(t *testing.T) {
	ctx := context.Background()
	open := &Limiter{Counter: failingCounter{}, Prefix: "p", Max: 1, Window: time.Minute}
	closed := &Limiter{Counter: failingCounter{}, Prefix: "p", Max: 1, Window: time.Minute, FailClosed: true}

	if out, _ := open.Take(ctx, "x"); out != Allow {
		t.Fatalf("fail-open Take: got %v, want Allow", out)
	}
	if out, err := closed.Take(ctx, "x"); out != Unavailable || err == nil {
		t.Fatalf("fail-closed Take: got %v (%v), want Unavailable with cause", out, err)
	}
	if out, _ := closed.CheckFailures(ctx, "x"); out != Unavailable {
		t.Fatal("fail-closed CheckFailures must be Unavailable")
	}
}

func TestMemoryCounter_WindowExpiryResets(t *testing.T) {
	c := NewMemoryCounter()
	ctx := context.Background()
	if n, _ := c.Incr(ctx, "k", time.Millisecond); n != 1 {
		t.Fatalf("first incr: %d", n)
	}
	time.Sleep(5 * time.Millisecond)
	if n, _ := c.Count(ctx, "k"); n != 0 {
		t.Fatalf("expired key should count 0, got %d", n)
	}
	if n, _ := c.Incr(ctx, "k", time.Minute); n != 1 {
		t.Fatalf("incr after expiry should restart at 1, got %d", n)
	}
}
