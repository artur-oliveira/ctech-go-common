package ws

import (
	"context"
	"errors"
	"testing"
)

type fakeConn struct {
	written [][]byte
	failAt  int // WriteMessage fails once written reaches this count
}

func (f *fakeConn) WriteMessage(_ int, data []byte) error {
	if f.failAt > 0 && len(f.written) >= f.failAt {
		return errors.New("write failed")
	}
	f.written = append(f.written, data)
	return nil
}

func TestMemoryRegistryBroadcastReachesRegisteredConn(t *testing.T) {
	r := NewMemoryRegistry()
	c := &fakeConn{}
	r.Register("key1", "conn1", c)
	r.Broadcast(context.Background(), "key1", []byte(`{"type":"deposit_confirmed"}`))
	if len(c.written) != 1 {
		t.Fatalf("expected 1 message, got %d", len(c.written))
	}
}

func TestMemoryRegistryBroadcastIgnoresOtherKeys(t *testing.T) {
	r := NewMemoryRegistry()
	c := &fakeConn{}
	r.Register("key1", "conn1", c)
	r.Broadcast(context.Background(), "key2", []byte(`{}`))
	if len(c.written) != 0 {
		t.Fatalf("expected 0 messages for a different key, got %d", len(c.written))
	}
}

func TestMemoryRegistryUnregisterStopsDelivery(t *testing.T) {
	r := NewMemoryRegistry()
	c := &fakeConn{}
	r.Register("key1", "conn1", c)
	r.Unregister("key1", "conn1")
	r.Broadcast(context.Background(), "key1", []byte(`{}`))
	if len(c.written) != 0 {
		t.Fatalf("expected 0 messages after unregister, got %d", len(c.written))
	}
}

func TestMemoryRegistryDeadConnIsRemoved(t *testing.T) {
	r := NewMemoryRegistry()
	c := &fakeConn{failAt: 1}
	r.Register("key1", "conn1", c)
	r.Broadcast(context.Background(), "key1", []byte(`{}`)) // write #1 succeeds
	r.Broadcast(context.Background(), "key1", []byte(`{}`)) // write #2 fails, conn is unregistered
	r.Broadcast(context.Background(), "key1", []byte(`{}`)) // no-op, already unregistered
	if len(c.written) != 1 {
		t.Fatalf("expected exactly 1 successful write before failure, got %d", len(c.written))
	}
}
