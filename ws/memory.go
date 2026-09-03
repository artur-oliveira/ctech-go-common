package ws

import (
	"context"
	"encoding/binary"
	"log/slog"
	"sync"
)

const (
	closeMessageType = 8
	goingAwayCode   = 1001
)

var goingAwayPayload = func() []byte {
	payload := make([]byte, 2+len("server restarting"))
	binary.BigEndian.PutUint16(payload, goingAwayCode)
	copy(payload[2:], "server restarting")
	return payload
}()

// MemoryRegistry is a single-instance connection registry. Use RedisRegistry
// to fan out across multiple API instances.
type MemoryRegistry struct {
	mu    sync.Mutex
	conns map[string]map[string]Conn // key → connID → conn
	draining bool
}

func NewMemoryRegistry() *MemoryRegistry {
	return &MemoryRegistry{conns: make(map[string]map[string]Conn)}
}

// Start is a no-op; MemoryRegistry has no background process to run.
func (m *MemoryRegistry) Start(_ context.Context) error { return nil }

// Stop rejects new registrations and asks connected clients to reconnect.
func (m *MemoryRegistry) Stop(ctx context.Context) error {
	m.mu.Lock()
	if m.draining {
		m.mu.Unlock()
		return nil
	}
	m.draining = true
	connections := make([]Conn, 0)
	for _, group := range m.conns {
		for _, conn := range group {
			connections = append(connections, conn)
		}
	}
	m.conns = make(map[string]map[string]Conn)
	m.mu.Unlock()

	for _, conn := range connections {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := conn.WriteMessage(closeMessageType, goingAwayPayload); err != nil {
			slog.Debug("ws graceful close failed", "err", err)
		}
	}
	return nil
}

func (m *MemoryRegistry) Register(key, connID string, conn Conn) {
	m.mu.Lock()
	if m.draining {
		m.mu.Unlock()
		_ = conn.WriteMessage(closeMessageType, goingAwayPayload)
		return
	}
	defer m.mu.Unlock()
	if _, ok := m.conns[key]; !ok {
		m.conns[key] = make(map[string]Conn)
	}
	m.conns[key][connID] = conn
	slog.Debug("ws registered", "key", key, "conn", connID)
}

func (m *MemoryRegistry) Unregister(key, connID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if group, ok := m.conns[key]; ok {
		delete(group, connID)
		if len(group) == 0 {
			delete(m.conns, key)
		}
	}
	slog.Debug("ws unregistered", "key", key, "conn", connID)
}

func (m *MemoryRegistry) Broadcast(_ context.Context, key string, payload []byte) {
	m.mu.Lock()
	group, ok := m.conns[key]
	if !ok {
		m.mu.Unlock()
		return
	}
	snapshot := make(map[string]Conn, len(group))
	for id, c := range group {
		snapshot[id] = c
	}
	m.mu.Unlock()

	for id, c := range snapshot {
		if err := c.WriteMessage(1, payload); err != nil {
			slog.Warn("ws send failed", "key", key, "conn", id, "err", err)
			m.Unregister(key, id)
		}
	}
}
