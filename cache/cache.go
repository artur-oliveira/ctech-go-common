// Package cache provides a CacheBackend interface with Redis and in-memory implementations.
package cache

import "context"

// Backend is the common cache interface.
type Backend interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Set(ctx context.Context, key string, value []byte, ttlSeconds int) error
	Delete(ctx context.Context, key string) error
	DeletePrefix(ctx context.Context, prefix string) error
	Ping(ctx context.Context) error
}
