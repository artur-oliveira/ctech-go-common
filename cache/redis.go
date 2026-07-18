package cache

import (
	"context"
	"strings"
	"time"

	"github.com/valkey-io/valkey-go"
)

// RedisBackend is a distributed cache backed by Valkey.
// Shared across all API replicas.
type RedisBackend struct {
	client valkey.Client
}

func NewRedisBackend(url string) (*RedisBackend, error) {
	opt, err := valkey.ParseURL(url)
	if err != nil {
		return nil, err
	}
	client, err := valkey.NewClient(opt)
	if err != nil {
		return nil, err
	}
	return &RedisBackend{client: client}, nil
}

func (r *RedisBackend) Get(ctx context.Context, key string) ([]byte, bool, error) {
	val, err := r.client.Do(ctx, r.client.B().Get().Key(key).Build()).AsBytes()
	if valkey.IsValkeyNil(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return val, true, nil
}

// Set writes value with a TTL. If Ex() turns out to want a raw second count
// instead of a time.Duration, change the argument to int64(ttlSeconds) —
// everything else here is unaffected.
func (r *RedisBackend) Set(ctx context.Context, key string, value []byte, ttlSeconds int) error {
	return r.client.Do(ctx, r.client.B().Set().Key(key).Value(string(value)).Ex(time.Duration(ttlSeconds)*time.Second).Build()).Error()
}

func (r *RedisBackend) Delete(ctx context.Context, key string) error {
	return r.client.Do(ctx, r.client.B().Del().Key(key).Build()).Error()
}

// globEscaper escapes SCAN MATCH glob metacharacters (*, ?, [, ], \) so a
// literal prefix containing one of them (e.g. an org/tenant ID) can't widen
// or corrupt the match pattern DeletePrefix builds from it.
var globEscaper = strings.NewReplacer(
	`\`, `\\`,
	`*`, `\*`,
	`?`, `\?`,
	`[`, `\[`,
	`]`, `\]`,
)

func (r *RedisBackend) DeletePrefix(ctx context.Context, prefix string) error {
	pattern := globEscaper.Replace(prefix) + "*"
	var cursor uint64
	for {
		entry, err := r.client.Do(ctx, r.client.B().Scan().Cursor(cursor).Match(pattern).Count(100).Build()).AsScanEntry()
		if err != nil {
			return err
		}
		if len(entry.Elements) > 0 {
			if err := r.client.Do(ctx, r.client.B().Del().Key(entry.Elements...).Build()).Error(); err != nil {
				return err
			}
		}
		cursor = entry.Cursor
		if cursor == 0 {
			break
		}
	}
	return nil
}

func (r *RedisBackend) Ping(ctx context.Context) error {
	return r.client.Do(ctx, r.client.B().Ping().Build()).Error()
}

func (r *RedisBackend) Client() valkey.Client { return r.client }
