package cache

import (
	"context"
	"testing"
	"time"
)

func TestMemoryBackendSetGet(t *testing.T) {
	b := NewMemoryBackend(10)
	ctx := context.Background()
	if err := b.Set(ctx, "k1", []byte("v1"), 60); err != nil {
		t.Fatalf("Set: %v", err)
	}
	val, ok, err := b.Get(ctx, "k1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok || string(val) != "v1" {
		t.Fatalf("Get = (%q, %v), want (v1, true)", val, ok)
	}
}

func TestMemoryBackendGetMissing(t *testing.T) {
	b := NewMemoryBackend(10)
	_, ok, err := b.Get(context.Background(), "nope")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for missing key")
	}
}

func TestMemoryBackendExpiry(t *testing.T) {
	b := NewMemoryBackend(10)
	ctx := context.Background()
	_ = b.Set(ctx, "k1", []byte("v1"), 0) // ttlSeconds=0 -> expiresAt = now, already expired on next check
	time.Sleep(time.Millisecond)
	_, ok, err := b.Get(ctx, "k1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Fatal("expected expired key to be absent")
	}
}

func TestMemoryBackendDelete(t *testing.T) {
	b := NewMemoryBackend(10)
	ctx := context.Background()
	_ = b.Set(ctx, "k1", []byte("v1"), 60)
	if err := b.Delete(ctx, "k1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, ok, _ := b.Get(ctx, "k1")
	if ok {
		t.Fatal("expected key gone after Delete")
	}
}

func TestMemoryBackendDeletePrefix(t *testing.T) {
	b := NewMemoryBackend(10)
	ctx := context.Background()
	_ = b.Set(ctx, "org:1:a", []byte("v1"), 60)
	_ = b.Set(ctx, "org:1:b", []byte("v2"), 60)
	_ = b.Set(ctx, "org:2:a", []byte("v3"), 60)
	if err := b.DeletePrefix(ctx, "org:1:"); err != nil {
		t.Fatalf("DeletePrefix: %v", err)
	}
	if _, ok, _ := b.Get(ctx, "org:1:a"); ok {
		t.Fatal("expected org:1:a gone")
	}
	if _, ok, _ := b.Get(ctx, "org:2:a"); !ok {
		t.Fatal("expected org:2:a to remain")
	}
}

func TestMemoryBackendPing(t *testing.T) {
	b := NewMemoryBackend(10)
	if err := b.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestMemoryBackendMaxSizeIsHardCap(t *testing.T) {
	// All entries carry a long TTL (none expired) — an eviction strategy that
	// only reclaims expired entries would let this grow past maxSize forever.
	b := NewMemoryBackend(2)
	ctx := context.Background()
	_ = b.Set(ctx, "k1", []byte("v1"), 3600)
	_ = b.Set(ctx, "k2", []byte("v2"), 3600)
	_ = b.Set(ctx, "k3", []byte("v3"), 3600)
	if len(b.entries) > 2 {
		t.Fatalf("entries = %d, want <= maxSize (2)", len(b.entries))
	}
}

func TestMemoryBackendUpdatingExistingKeyDoesNotEvict(t *testing.T) {
	b := NewMemoryBackend(2)
	ctx := context.Background()
	_ = b.Set(ctx, "k1", []byte("v1"), 3600)
	_ = b.Set(ctx, "k2", []byte("v2"), 3600)
	_ = b.Set(ctx, "k1", []byte("v1-updated"), 3600) // re-set an existing key at capacity
	if _, ok, _ := b.Get(ctx, "k2"); !ok {
		t.Fatal("expected k2 to survive an update to a different existing key")
	}
	val, ok, _ := b.Get(ctx, "k1")
	if !ok || string(val) != "v1-updated" {
		t.Fatalf("Get(k1) = (%q, %v), want (v1-updated, true)", val, ok)
	}
}
