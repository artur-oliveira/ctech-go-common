package supervisor

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.aoctech.app/api-commons/cache"
	"gopkg.aoctech.app/api-commons/drain"
	"gopkg.aoctech.app/api-commons/lock"
)

const (
	fast    = 5 * time.Millisecond
	waitFor = 3 * time.Second
)

func fastCfg() Config {
	return Config{MinBackoff: fast, MaxBackoff: 4 * fast, LeaderRetry: fast, HeartbeatInterval: fast}
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitFor)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRestartsAfterErrorAndPanicThenStopsOnNil(t *testing.T) {
	tests := []struct {
		name string
		fail func() error
	}{
		{"error", func() error { return errors.New("boom") }},
		{"panic", func() error { panic("boom") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := New(context.Background(), fastCfg())
			var calls atomic.Int32
			err := s.Run(context.Background(), "w", func(context.Context) error {
				if calls.Add(1) < 3 {
					return tc.fail()
				}
				return nil // finished: must not restart
			})
			if err != nil {
				t.Fatal(err)
			}
			eventually(t, func() bool { return calls.Load() == 3 })
			time.Sleep(10 * fast)
			if calls.Load() != 3 {
				t.Fatalf("restarted after nil: %d calls", calls.Load())
			}
			if err := s.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestShutdownIsCooperativeAndBounded(t *testing.T) {
	s := New(context.Background(), fastCfg())
	var exited atomic.Bool
	started := make(chan struct{})
	_ = s.Run(context.Background(), "coop", func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		exited.Store(true)
		return ctx.Err()
	})
	<-started
	if err := s.Shutdown(context.Background()); err != nil || !exited.Load() {
		t.Fatalf("err=%v exited=%v", err, exited.Load())
	}
	if err := s.Run(context.Background(), "late", func(context.Context) error { return nil }); !errors.Is(err, ErrStopped) {
		t.Fatalf("Run after Shutdown = %v", err)
	}

	// An uncooperative worker makes Shutdown return at the caller's deadline.
	s2 := New(context.Background(), fastCfg())
	release := make(chan struct{})
	defer close(release)
	stuck := make(chan struct{})
	_ = s2.Run(context.Background(), "stuck", func(context.Context) error { close(stuck); <-release; return nil })
	<-stuck
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := s2.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown = %v", err)
	}
}

func TestRunValidatesArgs(t *testing.T) {
	s := New(context.Background(), fastCfg())
	defer s.Shutdown(context.Background())
	if s.Run(context.Background(), "", func(context.Context) error { return nil }) == nil ||
		s.Run(context.Background(), "x", nil) == nil {
		t.Fatal("expected validation errors")
	}
}

func TestOnlyLeaderRunsAndFailoverOnShutdown(t *testing.T) {
	locker := lock.New(cache.NewMemoryBackend(16), time.Second) // shared by both "instances"
	cfg := fastCfg()
	cfg.Locker = locker
	a, b := New(context.Background(), cfg), New(context.Background(), cfg)
	var running, maxRunning, runsA, runsB atomic.Int32
	worker := func(runs *atomic.Int32) func(context.Context) error {
		return func(ctx context.Context) error {
			runs.Add(1)
			n := running.Add(1)
			defer running.Add(-1)
			if n > maxRunning.Load() {
				maxRunning.Store(n)
			}
			<-ctx.Done()
			return nil
		}
	}
	_ = a.Run(context.Background(), "singleton", worker(&runsA))
	eventually(t, func() bool { return runsA.Load() == 1 })
	_ = b.Run(context.Background(), "singleton", worker(&runsB))
	time.Sleep(10 * fast)
	if runsB.Load() != 0 || maxRunning.Load() != 1 {
		t.Fatalf("follower ran: B=%d max=%d", runsB.Load(), maxRunning.Load())
	}
	_ = a.Shutdown(context.Background())
	eventually(t, func() bool { return runsB.Load() == 1 })
	_ = b.Shutdown(context.Background())
}

func TestDrainStopsWorkersAndRefusesNew(t *testing.T) {
	var tr drain.Tracker
	cfg := fastCfg()
	cfg.Drain = &tr
	s := New(context.Background(), cfg)
	started := make(chan struct{})
	var exited atomic.Bool
	_ = s.Run(context.Background(), "w", func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		exited.Store(true)
		return nil
	})
	<-started
	if err := tr.Drain(context.Background()); err != nil || !exited.Load() {
		t.Fatalf("drain err=%v exited=%v", err, exited.Load())
	}
	if err := s.Run(context.Background(), "x", func(context.Context) error { return nil }); !errors.Is(err, ErrStopped) {
		t.Fatalf("Run after drain = %v", err)
	}
}

func TestEveryTicksAndSurvivesTickErrors(t *testing.T) {
	var n atomic.Int32
	s := New(context.Background(), fastCfg())
	_ = s.Run(context.Background(), "tick", Every(fast, func(context.Context) error {
		n.Add(1)
		return errors.New("transient")
	}))
	eventually(t, func() bool { return n.Load() >= 3 })
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestParentCancelStopsWorkers(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	s := New(parent, fastCfg())
	var exited atomic.Bool
	started := make(chan struct{})
	_ = s.Run(context.Background(), "w", func(ctx context.Context) error { close(started); <-ctx.Done(); exited.Store(true); return nil })
	<-started
	cancel()
	eventually(t, exited.Load)
}
