package ws

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/valkey-io/valkey-go"
)

const channelPrefix = "ws:"

// valkeyPubSub is the subset of valkey.Client that RedisRegistry needs.
// Narrowed so tests can fake it without implementing all of valkey.Client's
// methods (DoCache, DoStream, Dedicate, Nodes, ...) — valkey.Client already
// satisfies this interface structurally, so NewRedisRegistry's signature is
// unaffected.
type valkeyPubSub interface {
	B() valkey.Builder
	Do(ctx context.Context, cmd valkey.Completed) valkey.ValkeyResult
	Receive(ctx context.Context, subscribe valkey.Completed, fn func(msg valkey.PubSubMessage)) error
}

// RedisRegistry fans out WebSocket messages across all API instances via
// Valkey Pub/Sub. Each instance holds local connections; Valkey is the
// fan-out bus.
type RedisRegistry struct {
	client   valkeyPubSub
	local    *MemoryRegistry
	cancelFn context.CancelFunc
}

func NewRedisRegistry(client valkey.Client) *RedisRegistry {
	return &RedisRegistry{
		client: client,
		local:  NewMemoryRegistry(),
	}
}

func (r *RedisRegistry) Start(ctx context.Context) error {
	listenCtx, cancel := context.WithCancel(ctx)
	r.cancelFn = cancel
	go r.listen(listenCtx)
	slog.Info("RedisRegistry started")
	return nil
}

func (r *RedisRegistry) Stop(_ context.Context) error {
	if r.cancelFn != nil {
		r.cancelFn()
	}
	slog.Info("RedisRegistry stopped")
	return nil
}

func (r *RedisRegistry) Register(key, connID string, conn Conn) {
	r.local.Register(key, connID, conn)
}

func (r *RedisRegistry) Unregister(key, connID string) {
	r.local.Unregister(key, connID)
}

// Broadcast publishes to Valkey; the listener delivers to local connections.
func (r *RedisRegistry) Broadcast(ctx context.Context, key string, payload []byte) {
	ch := channelPrefix + key
	err := r.client.Do(ctx, r.client.B().Publish().Channel(ch).Message(string(payload)).Build()).Error()
	if err != nil {
		slog.Error("valkey publish failed, falling back to local", "key", key, "err", err)
		r.local.Broadcast(ctx, key, payload)
	}
}

// listen blocks on client.Receive, which itself blocks until: the
// subscription ends cleanly (returns nil — e.g. the client's underlying
// connection was recycled, or the server sent an unsubscribe/punsubscribe
// push unrelated to a deliberate shutdown), the client is closed manually
// (returns valkey.ErrClosing), ctx is done (returns ctx.Err()), or the
// connection drops (returns a non-nil err). A nil return does NOT mean the
// server wants the subscription gone for good — per valkey.Client.Receive's
// own doc comment, callers must re-subscribe to stay persistently listening.
// Only ctx cancellation or an explicit client Close() end the loop.
func (r *RedisRegistry) listen(ctx context.Context) {
	retryDelay := time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		err := r.client.Receive(ctx, r.client.B().Psubscribe().Pattern(channelPrefix+"*").Build(), func(msg valkey.PubSubMessage) {
			retryDelay = time.Second
			key := msg.Channel[len(channelPrefix):]
			r.local.Broadcast(ctx, key, []byte(msg.Message))
		})

		if errors.Is(err, valkey.ErrClosing) || ctx.Err() != nil {
			return
		}

		slog.Warn("valkey pubsub closed, resubscribing", "delay", retryDelay, "err", err)
		time.Sleep(retryDelay)
		retryDelay = min(retryDelay*2, 60*time.Second)
	}
}
