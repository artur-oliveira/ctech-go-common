package drain

import (
	"context"
	"errors"
	"testing"
)

func TestDrainClosesSnapshotOnceAndRejectsNewConnections(t *testing.T) {
	var tracker Tracker
	closed := 0
	for _, id := range []string{"one", "two"} {
		if !tracker.Register(id, func(context.Context) error { closed++; return nil }) {
			t.Fatal("registration unexpectedly rejected")
		}
	}
	if err := tracker.Drain(context.Background()); err != nil {
		t.Fatalf("Drain() error = %v", err)
	}
	if err := tracker.Drain(context.Background()); err != nil {
		t.Fatalf("second Drain() error = %v", err)
	}
	if closed != 2 {
		t.Fatalf("closed = %d, want 2", closed)
	}
	if tracker.Register("late", func(context.Context) error { return nil }) {
		t.Fatal("late registration accepted")
	}
}

func TestDrainJoinsCloseErrors(t *testing.T) {
	var tracker Tracker
	want := errors.New("close failed")
	tracker.Register("one", func(context.Context) error { return want })
	if err := tracker.Drain(context.Background()); !errors.Is(err, want) {
		t.Fatalf("Drain() error = %v, want %v", err, want)
	}
}

func TestUnregisterRemovesConnection(t *testing.T) {
	var tracker Tracker
	called := false
	tracker.Register("gone", func(context.Context) error { called = true; return nil })
	tracker.Unregister("gone")
	if err := tracker.Drain(context.Background()); err != nil {
		t.Fatalf("Drain() error = %v", err)
	}
	if called {
		t.Fatal("unregistered connection was closed")
	}
}
