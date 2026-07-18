// Package ws implements a WebSocket connection registry fanned out across API
// instances via Redis Pub/Sub.
//
// Fan-out pattern:
//   - Each API instance holds a local map[key → []conn].
//   - A writer publishes to Redis channel "ws:{key}".
//   - All instances subscribed to that channel receive and push to local connections.
//   - No sticky sessions required.
//
// key is an opaque fan-out identifier chosen by the consumer (e.g. an
// organization ID for a multi-tenant service, a user ID for a single-tenant one).
package ws

import "context"

// Conn is a minimal WebSocket connection abstraction.
type Conn interface {
	WriteMessage(messageType int, data []byte) error
}

// Registry fans out payloads to WebSocket connections keyed by an opaque key.
type Registry interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Register(key, connID string, conn Conn)
	Unregister(key, connID string)
	Broadcast(ctx context.Context, key string, payload []byte)
}
