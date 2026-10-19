// Package supervisor runs long-lived background workers inside one process:
// cancellable context, optional distributed leadership, restart with backoff,
// cooperative shutdown and drain.Tracker integration. See README.md.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"gopkg.aoctech.app/api-commons/drain"
	"gopkg.aoctech.app/api-commons/lock"
	"gopkg.aoctech.app/api-commons/observability"
)

// Defaults.
const (
	DefaultMinBackoff = time.Second
	DefaultMaxBackoff = time.Minute
	// DefaultLeaderRetry is how often a non-leader instance retries the lock.
	DefaultLeaderRetry = 15 * time.Second
	// DefaultLockPrefix namespaces leader keys: <prefix><worker name>. Include
	// the environment in it when a Valkey is shared across environments.
	DefaultLockPrefix = "supervisor:"
	// DefaultDrainID is the id registered in a drain.Tracker.
	DefaultDrainID = "supervisor"
)

// ErrStopped is returned by Run once shutdown or drain has started.
var ErrStopped = errors.New("supervisor: stopped")

// Config tunes a Supervisor. The zero value is usable.
type Config struct {
	// Locker enables leader election: a worker runs only on the instance holding
	// lock key LockPrefix+name. nil means every instance runs every worker.
	// The Locker's TTL must exceed HeartbeatInterval by a safe margin.
	Locker *lock.Locker
	// LockPrefix defaults to DefaultLockPrefix.
	LockPrefix string
	// HeartbeatInterval renews the leader lock; lock.DefaultHeartbeatInterval
	// when zero.
	HeartbeatInterval time.Duration
	// LeaderRetry defaults to DefaultLeaderRetry.
	LeaderRetry time.Duration
	// MinBackoff/MaxBackoff bound the restart delay after a failure.
	MinBackoff, MaxBackoff time.Duration
	// Drain, when set, stops all workers when the tracker drains and refuses new
	// ones afterwards.
	Drain *drain.Tracker
	// DrainID defaults to DefaultDrainID; set it when several supervisors share
	// one Tracker.
	DrainID string
}

// Supervisor owns the workers of one process.
type Supervisor struct {
	cfg    Config
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New returns a Supervisor whose workers also stop when parent is cancelled.
func New(parent context.Context, cfg Config) *Supervisor {
	if cfg.LockPrefix == "" {
		cfg.LockPrefix = DefaultLockPrefix
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = lock.DefaultHeartbeatInterval
	}
	if cfg.LeaderRetry <= 0 {
		cfg.LeaderRetry = DefaultLeaderRetry
	}
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = DefaultMinBackoff
	}
	if cfg.MaxBackoff < cfg.MinBackoff {
		cfg.MaxBackoff = max(DefaultMaxBackoff, cfg.MinBackoff)
	}
	if cfg.DrainID == "" {
		cfg.DrainID = DefaultDrainID
	}
	ctx, cancel := context.WithCancel(parent)
	s := &Supervisor{cfg: cfg, ctx: ctx, cancel: cancel}
	if cfg.Drain != nil {
		cfg.Drain.Register(cfg.DrainID, s.Shutdown) // false = already draining; Run refuses then
	}
	return s
}

// Run starts fn as a named worker and returns immediately. The context passed
// to fn is cancelled by ctx, by Shutdown, by a drain, or when leadership is
// lost; fn must return promptly when it is.
//
// fn returning nil means the work is finished: the worker ends and is not
// restarted. fn returning an error or panicking restarts it after a backoff.
func (s *Supervisor) Run(ctx context.Context, name string, fn func(ctx context.Context) error) error {
	if name == "" || fn == nil {
		return errors.New("supervisor: name and fn are required")
	}
	if s.stopped() {
		return ErrStopped
	}
	wctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		defer stop()
		s.loop(wctx, name, fn)
	}()
	return nil
}

// Shutdown cancels every worker and waits for them to return, or for ctx. On
// ctx expiry it returns ctx.Err() and the stragglers keep running (Go cannot
// kill them); the caller should exit the process. Safe to call repeatedly.
func (s *Supervisor) Shutdown(ctx context.Context) error {
	s.cancel()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Supervisor) stopped() bool {
	return s.ctx.Err() != nil || (s.cfg.Drain != nil && s.cfg.Drain.Draining())
}

func (s *Supervisor) loop(ctx context.Context, name string, fn func(context.Context) error) {
	backoff := s.cfg.MinBackoff
	for !s.stopped() && ctx.Err() == nil {
		started := time.Now()
		done, err := s.attempt(ctx, name, fn)
		if done || ctx.Err() != nil {
			return
		}
		delay := s.cfg.LeaderRetry
		if err != nil {
			if time.Since(started) >= s.cfg.MaxBackoff {
				backoff = s.cfg.MinBackoff // it was healthy for a while: not a crash loop
			}
			observability.Error(ctx, "supervisor: worker failed, restarting", err, "worker", name, "backoff", backoff.String())
			delay = backoff
			backoff = min(backoff*2, s.cfg.MaxBackoff)
		}
		if !sleep(ctx, delay) {
			return
		}
	}
}

// attempt acquires leadership (when configured) and runs fn once. done=true
// means fn finished cleanly. err!=nil asks for a backoff; err==nil && !done
// means "not leader, retry later".
func (s *Supervisor) attempt(ctx context.Context, name string, fn func(context.Context) error) (done bool, err error) {
	if s.cfg.Locker == nil {
		return runSafe(ctx, name, fn)
	}
	// Fail-closed: if the lock store is unreachable nobody leads, so a singleton
	// worker pauses instead of running on every instance.
	key := s.cfg.LockPrefix + name
	release, ok, lerr := s.cfg.Locker.Acquire(ctx, key)
	if lerr != nil {
		return false, fmt.Errorf("acquire leader lock %q: %w", key, lerr)
	}
	if !ok {
		return false, nil
	}
	defer release()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// A failed renewal (lost or store unreachable) cancels fn: losing authority
	// must stop the work immediately.
	stopHB := s.cfg.Locker.StartHeartbeat(runCtx, key, s.cfg.HeartbeatInterval, cancel)
	defer stopHB()
	done, err = runSafe(runCtx, name, fn)
	if err == nil && !done {
		return false, nil
	}
	return done, err
}

func runSafe(ctx context.Context, name string, fn func(context.Context) error) (done bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			done, err = false, fmt.Errorf("supervisor: worker %q panicked: %v", name, r)
		}
	}()
	if err = fn(ctx); err != nil {
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			return false, nil // cancelled, not a failure
		}
		return false, err
	}
	// nil while still cancelled by leadership loss would also land here; the
	// caller's ctx check distinguishes shutdown from completion.
	return ctx.Err() == nil, nil
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Every adapts a tick function to a worker: it calls tick every interval until
// ctx ends. A tick error is logged and the loop continues (the account
// workers' behaviour); a panic escapes and restarts the worker.
func Every(interval time.Duration, tick func(ctx context.Context) error) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-t.C:
				if err := tick(ctx); err != nil && ctx.Err() == nil {
					observability.Error(ctx, "supervisor: tick failed", err)
				}
			}
		}
	}
}
