// Package drain coordinates graceful shutdown of long-lived connections.
package drain

import (
	"context"
	"errors"
	"sync"
)

// CloseFunc asks a connection to reconnect elsewhere. Implementations should
// return promptly and honor ctx.
type CloseFunc func(ctx context.Context) error

// Tracker owns the live connections of one process. Its zero value is ready to
// use. Once Drain starts, new registrations are rejected permanently.
type Tracker struct {
	mu       sync.Mutex
	conns    map[string]CloseFunc
	draining bool
}

// Register adds or replaces a connection. False means shutdown has started.
func (t *Tracker) Register(id string, close CloseFunc) bool {
	if id == "" || close == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.draining {
		return false
	}
	if t.conns == nil {
		t.conns = make(map[string]CloseFunc)
	}
	t.conns[id] = close
	return true
}

// Unregister removes a connection. It is safe to call after Drain starts.
func (t *Tracker) Unregister(id string) {
	t.mu.Lock()
	delete(t.conns, id)
	t.mu.Unlock()
}

// Draining reports whether the process has stopped accepting connections.
func (t *Tracker) Draining() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.draining
}

// Drain atomically blocks registrations and asks every captured connection to
// reconnect. Repeated calls are safe and do not close a connection twice.
func (t *Tracker) Drain(ctx context.Context) error {
	t.mu.Lock()
	if t.draining {
		t.mu.Unlock()
		return nil
	}
	t.draining = true
	connections := make([]CloseFunc, 0, len(t.conns))
	for _, close := range t.conns {
		connections = append(connections, close)
	}
	t.conns = nil
	t.mu.Unlock()

	var errs []error
	for _, close := range connections {
		if err := close(ctx); err != nil {
			errs = append(errs, err)
		}
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
	}
	return errors.Join(errs...)
}
