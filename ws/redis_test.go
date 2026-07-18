package ws

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valkey-io/valkey-go"
)

// fakeValkeyPubSub drives RedisRegistry.listen() without a real Valkey
// server. A real miniredis server was considered, but forcing a mid-
// subscription connection drop there only produces a non-nil error from
// Receive() (the already-working path) — it cannot reproduce the specific
// "clean, nil-error" subscription end this test guards against, since that
// case is an internal valkey.Client behavior (e.g. connection recycling),
// not something a fake server can trigger over the wire.
type fakeValkeyPubSub struct {
	receiveCalls int32
	resubscribed chan struct{}
}

func (f *fakeValkeyPubSub) B() valkey.Builder { return valkey.Builder{} }

func (f *fakeValkeyPubSub) Do(_ context.Context, _ valkey.Completed) valkey.ValkeyResult {
	return valkey.ValkeyResult{}
}

// Receive simulates valkey.Client.Receive's documented case 1: it returns nil
// ("received any unsubscribe/punsubscribe message related to the provided
// subscribe command") on its first call, without ctx being done and without
// valkey.ErrClosing. That is not a request to stop listening for good — the
// second call proves listen() resubscribed instead of giving up.
func (f *fakeValkeyPubSub) Receive(ctx context.Context, _ valkey.Completed, _ func(msg valkey.PubSubMessage)) error {
	if atomic.AddInt32(&f.receiveCalls, 1) == 1 {
		return nil
	}
	close(f.resubscribed)
	<-ctx.Done()
	return ctx.Err()
}

func TestRedisRegistryListenResubscribesAfterCleanReceiveReturn(t *testing.T) {
	fc := &fakeValkeyPubSub{resubscribed: make(chan struct{})}
	r := &RedisRegistry{client: fc, local: NewMemoryRegistry()}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = r.Stop(context.Background()) }()

	select {
	case <-fc.resubscribed:
	case <-time.After(3 * time.Second):
		t.Fatal("listen() did not resubscribe after Receive returned nil — a clean subscription end must not be treated as a permanent stop")
	}

	if calls := atomic.LoadInt32(&fc.receiveCalls); calls < 2 {
		t.Fatalf("expected at least 2 Receive() calls, got %d", calls)
	}
}
