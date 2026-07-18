# ctech-go-common Extraction Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:
> executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `ctech-go-common`, a shared Go module reconciling the byte-identical/drifted internal packages currently
duplicated between `ctech-dfe/api` and `ctech-wallet/api` (`cache`, DynamoDB marshal/base helpers, RFC 7807 `problem`,
AWS config/DynamoDB client bootstrap, Redis pub/sub `ws` registry), published under the vanity import path
`gopkg.aoctech.app/api-commons` (served by the already-deployed `ctech-vanity` Cloudflare Worker).

**Architecture:** A single Go module with five focused packages (`cache`, `dynamo`, `problem`, `awsconfig`, `ws`), each
holding only the genuinely shared primitives — not the per-service authorization/business logic layered on top in each
consumer. Where `ctech-dfe` and `ctech-wallet` had already drifted (`base.go`'s `UpsertAttrs`, `BuildRawUpdateTxItem`,
`Decode`/`Encode`, and a `QueryGSI` reserved-word fix), the superset/fixed version is what ships here — both consumers
gain the missing capability when they migrate. This plan builds and tests the module in isolation; it does **not** touch
`ctech-dfe` or `ctech-wallet` — that migration is two separate follow-up plans, once this module is tagged and proven.

**Tech Stack:** Go 1.26, `aws-sdk-go-v2`, `github.com/valkey-io/valkey-go`, standard `testing`.

## Global Constraints

- Module path: `gopkg.aoctech.app/api-commons` (not the raw `github.com/artur-oliveira/ctech-go-common` path) — this is
  the entire point of `ctech-vanity`'s existence; `go get`/module-proxy resolution walks the `go-import` meta tag at
  that domain to the real GitHub repo. User explicitly confirmed this convention and that `ctech-dfe`/`ctech-wallet`
  will switch their own import paths to `gopkg.aoctech.app/*` too, in their own follow-up migration plans.
- Repo is public (`artur-oliveira/ctech-go-common` on GitHub, confirmed via `api.github.com` — `"private": false`), so
  default `GOPROXY=https://proxy.golang.org` works with no extra config once a semver tag is pushed.
- `go` toolchain lives at `~/sdk/go1.26.5/bin/go`, now on `PATH` (added to `~/.bashrc`). Use plain `go` in commands
  below.
- `go.mod` line: `go 1.26`.
- Do **not** port `CRUDRepository[T]` (the org-scoped generic CRUD wrapper in `ctech-dfe`'s `base.go`) — it's
  multi-tenant-specific and `ctech-wallet` explicitly has no equivalent (not multi-tenant, per its own `CLAUDE.md`).
  Only the primitive `Base` operations are shared.
- Do **not** unify the per-service `awsclient.Clients` struct — `ctech-dfe` wires S3/SQS/SNS/Lambda/SecretsManager,
  `ctech-wallet` wires only SSM; forcing one shape would be a false unification the original audit explicitly warned
  against. Only the AWS config-loading + DynamoDB-client-with-endpoint-override boilerplate is shared (package
  `awsconfig`).
- `ws` package: rename the identical `orgPK`/`userID` parameter to `key` — the logic is 100% identical in both repos (
  verified via `diff`, exit 0 on `memory.go`/`redis.go`, only comments/identifiers differ in `registry.go`/`redis.go`),
  the naming difference is cosmetic to what each service's channel actually carries.
- License: Elastic License 2.0 (ELv2), same text as `ctech-dfe`/`ctech-account`/`ctech-vanity`. Copyright holder: Artur
  Oliveira Carvalho.
- **Redis client library: `github.com/valkey-io/valkey-go` (package `valkey`), not `github.com/redis/go-redis/v9`.** The
  server runs Valkey natively. Module path confirmed via `go.mod` on the upstream repo:
  `module github.com/valkey-io/valkey-go` (no version suffix), requires `go 1.25.0`+. API is NOT a drop-in replacement
  for go-redis: commands go through a builder (`client.B().Get().Key(k).Build()` → `client.Do(ctx, cmd)`), Pub/Sub is
  callback-based (`client.Receive(ctx, cmd, func(msg valkey.PubSubMessage) {...})`, blocking until the subscription
  ends/errors) instead of a Go channel to range over, and nil detection uses `valkey.IsValkeyNil(err)` instead of
  `errors.Is(err, redis.Nil)`. Two call signatures below (`Set(...).Ex(...)`'s parameter type, and whether
  pattern-subscribe uses `.Pattern()` or `.Channel()`) came back inconsistent across two documentation sources during
  research — if `go build` fails on either call in Task 2/Task 6, run `go doc github.com/valkey-io/valkey-go <Type>` (or
  check `$(go env GOPATH)/pkg/mod/github.com/valkey-io/valkey-go@<version>/`) and fix the one call; this is the only
  place in this plan where an exact signature wasn't independently confirmed from source, and the fix is a one-line
  correction, not a design change.
- No placeholder code — every function below is either a verbatim port (marked "verbatim") or a real reconciliation of
  the two existing versions (marked "reconciled", with the source diff already resolved in this plan).

---

### Task 1: Repo scaffold

**Files:**

- Create: `go.mod`
- Create: `.gitignore`
- Create: `LICENSE.md`
- Create: `README.md`

**Interfaces:**

- Produces: module `gopkg.aoctech.app/api-commons`, ready for `go get`/package-level `go build` in later tasks.

- [ ] **Step 1: Init go.mod**

```bash
cd /home/artur/Documents/Projects/Ctech/ctech-go-common
go mod init gopkg.aoctech.app/api-commons
```

Expected `go.mod` content after init (edit the `go` line to match):

```
module gopkg.aoctech.app/api-commons

go 1.26
```

- [ ] **Step 2: .gitignore**

`.gitignore`:

```
/dist/
*.test
*.out
.idea/
```

- [ ] **Step 3: LICENSE.md** (verbatim ELv2, matching `ctech-dfe/LICENSE.md`)

```markdown
# Elastic License 2.0 (ELv2)

**Copyright (c) 2026 Artur Oliveira Carvalho**

## Acceptance

By using the software, you agree to all of the terms and conditions below.

## Copyright License

The licensor grants you a non-exclusive, royalty-free, worldwide, non-sublicensable, non-transferable license to use, copy, distribute, make available, and prepare derivative works of the software, in each case subject to the limitations and conditions below.

## Limitations

**You may not provide the software to third parties as a hosted or managed service**, where the service provides users with access to any substantial set of the features or functionality of the software.

You may not move, change, disable, or circumvent the license key functionality in the software, and you may not remove or obscure any functionality in the software that is protected by the license key.

You may not alter, remove, or obscure any licensing, copyright, or other notices of the licensor in the software. Any use of the licensor's trademarks is subject to applicable law.

## Patents

The licensor grants you a license, under any patent claims the licensor can license, or becomes able to license, to make, have made, use, sell, offer for sale, import and have imported the software, in each case subject to the limitations and conditions in this license. This license does not cover any patent claims that you cause to be infringed by modifications or additions to the software. If you or your company make any written claim that the software infringes or contributes to infringement of any patent, your patent license for the software granted under these terms ends immediately.

## Notices

You must ensure that anyone who gets a copy of any part of the software from you also gets a copy of these terms.

If you modify the software, you must include in any modified copies of the software prominent notices stating that you have modified the software.

## No Other Rights

These terms do not imply any licenses other than those expressly granted in these terms.

## Termination

If you use the software in violation of these terms, such use is not licensed, and your licenses will automatically terminate. If the licensor provides you with a notice of your violation, and you cease all violation of this license no later than 30 days after you receive that notice, your licenses will be reinstated retroactively. However, if you violate these terms after such reinstatement, any additional violation of these terms will cause your licenses to terminate automatically and permanently.

## No Liability

*As far as the law allows, the software comes as is, without any warranty or condition, and the licensor will not be liable to you for any damages arising out of these terms or the use or nature of the software, under any kind of legal claim.*

## Definitions

The **licensor** is the entity offering these terms, and the **software** is the software the licensor makes available under these terms, including any portion of it.

**You** refers to the individual or entity agreeing to these terms.

**Your company** is any legal entity, sole proprietorship, or other kind of organization that you work for, plus all organizations that have control over, are under the control of, or are under common control with that organization.

**Control** means ownership of substantially all the assets of an entity, or the power to direct its management and policies by vote, contract, or otherwise. Control can be direct or indirect.

**Your licenses** are all the licenses granted to you for the software under these terms.

**Use** means anything you do with the software requiring one of your licenses.

**Trademark** means trademarks, service marks, and similar rights.
```

- [ ] **Step 4: README.md** (placeholder, expanded in Task 7)

```markdown
# ctech-go-common

Shared Go module for the CTech platform — reconciles internal packages duplicated between
`ctech-dfe/api` and `ctech-wallet/api`. Imported as `gopkg.aoctech.app/api-commons` (served by
[`ctech-vanity`](https://github.com/artur-oliveira/ctech-vanity)'s go-import redirect), not the
raw GitHub path.

Status: under construction — see `docs/superpowers/plans/2026-07-17-ctech-go-common-extraction.md`.

## License

[Elastic License 2.0 (ELv2)](LICENSE.md).
```

- [ ] **Step 5: Commit**

```bash
git add go.mod .gitignore LICENSE.md README.md
git commit -m "chore: scaffold ctech-go-common module"
```

---

### Task 2: `cache` package

**Files:**

- Create: `cache/cache.go`
- Create: `cache/memory.go`
- Create: `cache/redis.go`
- Test: `cache/memory_test.go`

**Interfaces:**

- Produces: `cache.Backend` interface, `cache.NewMemoryBackend(maxSize int) *MemoryBackend`,
  `cache.NewRedisBackend(url string) (*RedisBackend, error)`.

- [ ] **Step 1: Add valkey-go dependency**

```bash
cd /home/artur/Documents/Projects/Ctech/ctech-go-common
go get github.com/valkey-io/valkey-go@latest
```

- [ ] **Step 2: Write the failing test**

`cache/memory_test.go`:

```go
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
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./cache/... -run TestMemoryBackend -v`
Expected: FAIL — `cache.go`/`memory.go`/`redis.go` don't exist yet (`no Go files in .../cache`).

- [ ] **Step 4: Write cache.go (verbatim from ctech-dfe/api/internal/cache/cache.go — byte-identical to ctech-wallet's
  copy)**

`cache/cache.go`:

```go
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
```

- [ ] **Step 5: Write memory.go (verbatim)**

`cache/memory.go`:

```go
package cache

import (
	"context"
	"strings"
	"sync"
	"time"
)

type memEntry struct {
	value     []byte
	expiresAt time.Time
}

// MemoryBackend is a single-instance in-memory cache.
// Not shared across replicas — use RedisBackend in multi-instance deployments.
type MemoryBackend struct {
	mu      sync.Mutex
	entries map[string]memEntry
	maxSize int
}

func NewMemoryBackend(maxSize int) *MemoryBackend {
	if maxSize <= 0 {
		maxSize = 1000
	}
	return &MemoryBackend{entries: make(map[string]memEntry, maxSize), maxSize: maxSize}
}

func (m *MemoryBackend) Get(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[key]
	if !ok {
		return nil, false, nil
	}
	if time.Now().After(e.expiresAt) {
		delete(m.entries, key)
		return nil, false, nil
	}
	return e.value, true, nil
}

func (m *MemoryBackend) Set(_ context.Context, key string, value []byte, ttlSeconds int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.entries) >= m.maxSize {
		// evict one expired entry (simple strategy)
		for k, e := range m.entries {
			if time.Now().After(e.expiresAt) {
				delete(m.entries, k)
				break
			}
		}
	}
	m.entries[key] = memEntry{
		value:     value,
		expiresAt: time.Now().Add(time.Duration(ttlSeconds) * time.Second),
	}
	return nil
}

func (m *MemoryBackend) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, key)
	return nil
}

func (m *MemoryBackend) DeletePrefix(_ context.Context, prefix string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.entries {
		if strings.HasPrefix(k, prefix) {
			delete(m.entries, k)
		}
	}
	return nil
}

func (m *MemoryBackend) Ping(_ context.Context) error { return nil }
```

- [ ] **Step 6: Write redis.go (valkey-go, not go-redis — see Global Constraints)**

`cache/redis.go`:

```go
package cache

import (
	"context"
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
// instead of a time.Duration (see Global Constraints), change the argument to
// int64(ttlSeconds) — everything else here is unaffected.
func (r *RedisBackend) Set(ctx context.Context, key string, value []byte, ttlSeconds int) error {
	return r.client.Do(ctx, r.client.B().Set().Key(key).Value(string(value)).Ex(time.Duration(ttlSeconds)*time.Second).Build()).Error()
}

func (r *RedisBackend) Delete(ctx context.Context, key string) error {
	return r.client.Do(ctx, r.client.B().Del().Key(key).Build()).Error()
}

func (r *RedisBackend) DeletePrefix(ctx context.Context, prefix string) error {
	var cursor uint64
	for {
		entry, err := r.client.Do(ctx, r.client.B().Scan().Cursor(cursor).Match(prefix+"*").Count(100).Build()).AsScanEntry()
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
```

- [ ] **Step 7: Run test to verify it passes**

Run: `go test ./cache/... -v`
Expected: PASS (6 tests)

- [ ] **Step 8: Commit**

```bash
git add go.mod go.sum cache/
git commit -m "feat: add cache package (memory + redis backends)"
```

---

### Task 3: `dynamo` package

**Files:**

- Create: `dynamo/marshal.go`
- Create: `dynamo/base.go`
- Test: `dynamo/marshal_test.go`
- Test: `dynamo/base_test.go`

**Interfaces:**

- Produces: `dynamo.MarshalMapOmitNull(v any) (map[string]types.AttributeValue, error)`, `dynamo.Base` struct with
  `NewBase(db *dynamodb.Client, tablePrefix, table string) Base` (decoupled from any consumer's `config.Config` — takes
  a plain `tablePrefix string`, consumers pass `cfg.TablePrefix`), `dynamo.TableName(tablePrefix, table string) string`,
  `dynamo.NowStr() string`, `dynamo.QueryOpts`, `dynamo.QueryResult`, `dynamo.Decode[T any]`, `dynamo.Encode`,
  `dynamo.IsConditionFailed(err error) bool`, plus all `Base` methods listed in Step 3 below.
- Consumes: `cache` package not required here (independent package).

- [ ] **Step 1: Add aws-sdk-go-v2 dependencies**

```bash
cd /home/artur/Documents/Projects/Ctech/ctech-go-common
go get github.com/aws/aws-sdk-go-v2@v1.42.1
go get github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue@v1.20.52
go get github.com/aws/aws-sdk-go-v2/service/dynamodb@v1.60.1
```

- [ ] **Step 2: Write the failing tests**

`dynamo/marshal_test.go` (verbatim from `ctech-dfe/api/internal/repositories/marshal_test.go`):

```go
package dynamo

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestMarshalMapOmitNull(t *testing.T) {
	t.Run("top-level nulls omitted", func(t *testing.T) {
		in := map[string]any{
			"name":  "Vasilhame",
			"cest":  nil,
			"value": "20.00",
			"empty": "",
		}
		out, err := MarshalMapOmitNull(in)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := out["cest"]; ok {
			t.Errorf("expected null attribute 'cest' to be omitted, got %#v", out["cest"])
		}
		if _, ok := out["name"]; !ok {
			t.Errorf("expected 'name' to be present")
		}
		if s, ok := out["empty"].(*types.AttributeValueMemberS); !ok || s.Value != "" {
			t.Errorf("expected empty string to be preserved, got %#v", out["empty"])
		}
	})

	t.Run("nested nulls omitted", func(t *testing.T) {
		in := map[string]any{
			"name": "X",
			"addr": map[string]any{"city": "POA", "complement": nil},
			"list": []any{map[string]any{"x": "1", "y": nil}},
		}
		out, err := MarshalMapOmitNull(in)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		addrAV, ok := out["addr"].(*types.AttributeValueMemberM)
		if !ok {
			t.Fatalf("expected 'addr' to be a map, got %T", out["addr"])
		}
		if _, exists := addrAV.Value["complement"]; exists {
			t.Errorf("expected nested null 'addr.complement' to be omitted")
		}
		if _, exists := addrAV.Value["city"]; !exists {
			t.Errorf("expected 'addr.city' to be present")
		}

		listAV, ok := out["list"].(*types.AttributeValueMemberL)
		if !ok {
			t.Fatalf("expected 'list' to be a list, got %T", out["list"])
		}
		if len(listAV.Value) == 0 {
			t.Fatalf("expected list to have one element")
		}
		elemAV, ok := listAV.Value[0].(*types.AttributeValueMemberM)
		if !ok {
			t.Fatalf("expected list element to be a map, got %T", listAV.Value[0])
		}
		if _, exists := elemAV.Value["y"]; exists {
			t.Errorf("expected nested null 'list[0].y' to be omitted")
		}
		if _, exists := elemAV.Value["x"]; !exists {
			t.Errorf("expected 'list[0].x' to be present")
		}
	})
}
```

`dynamo/base_test.go` (merges dfe's transact-item builder tests + wallet's `NewBase`/`buildUpdateExpr` tests, adapted
for the decoupled `tablePrefix string` signature):

```go
package dynamo

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestNewBasePrefixesTable(t *testing.T) {
	b := NewBase(nil, "test", "wallets")
	if b.TableName != "test_wallets" {
		t.Fatalf("TableName = %q, want %q", b.TableName, "test_wallets")
	}
}

func TestBuildUpdateExpr_SetAndRemove(t *testing.T) {
	expr, names, values, err := buildUpdateExpr(map[string]any{
		"name": "X",
		"cest": nil,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(expr, "SET #name = :name") {
		t.Errorf("expected SET clause for name, got %q", expr)
	}
	if !strings.Contains(expr, "REMOVE #cest") {
		t.Errorf("expected REMOVE clause for cest, got %q", expr)
	}
	if _, ok := values[":cest"]; ok {
		t.Errorf("nil value must not be in ExpressionAttributeValues")
	}
	if names["#cest"] != "cest" {
		t.Errorf("expected name mapping for cest")
	}
}

func TestBuildUpdateExpr_RemoveOnly(t *testing.T) {
	expr, _, values, err := buildUpdateExpr(map[string]any{"cest": nil})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(expr, "SET") {
		t.Errorf("expected no SET clause, got %q", expr)
	}
	if !strings.HasPrefix(expr, "REMOVE") {
		t.Errorf("expected REMOVE-only expression, got %q", expr)
	}
	if len(values) != 0 {
		t.Errorf("expected no expression values, got %d", len(values))
	}
}

func TestBase_BuildPutTxItem(t *testing.T) {
	b := Base{TableName: "test_table"}
	item := map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: "PK1"},
		"sk": &types.AttributeValueMemberS{Value: "SK1"},
	}
	txItem := b.BuildPutTxItem(item)
	if txItem.Put == nil {
		t.Fatal("expected Put transact item, got nil")
	}
	if *txItem.Put.TableName != b.TableName {
		t.Errorf("table name = %q, want %q", *txItem.Put.TableName, b.TableName)
	}
	if txItem.Put.Item["pk"].(*types.AttributeValueMemberS).Value != "PK1" {
		t.Error("item not carried through unchanged")
	}
}

func TestBase_BuildUpdateTxItem(t *testing.T) {
	b := Base{TableName: "test_table"}
	txItem, err := b.BuildUpdateTxItem("PK1", new("SK1"), map[string]any{"name": "new-name"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if txItem.Update == nil {
		t.Fatal("expected Update transact item, got nil")
	}
	if *txItem.Update.ConditionExpression != "attribute_exists(pk)" {
		t.Errorf("condition = %q, want attribute_exists(pk)", *txItem.Update.ConditionExpression)
	}
	if txItem.Update.Key["sk"].(*types.AttributeValueMemberS).Value != "SK1" {
		t.Error("sk not set on key")
	}
}

func TestBase_BuildDeleteTxItem(t *testing.T) {
	b := Base{TableName: "test_table"}
	txItem := b.BuildDeleteTxItem("PK1", "SK1")
	if txItem.Delete == nil {
		t.Fatal("expected Delete transact item, got nil")
	}
	if *txItem.Delete.ConditionExpression != "attribute_exists(pk)" {
		t.Errorf("condition = %q, want attribute_exists(pk)", *txItem.Delete.ConditionExpression)
	}
}

func TestBase_UpsertAttrs_NoConditionExpression(t *testing.T) {
	// UpsertAttrs must NOT carry attribute_exists(pk) — that's the entire point:
	// it creates the row on first write instead of failing when absent.
	expr, names, values, err := buildUpdateExpr(map[string]any{"consent_a": "2026-07-17"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(expr, "SET #consent_a = :consent_a") {
		t.Errorf("expected SET clause, got %q", expr)
	}
	if names["#consent_a"] != "consent_a" {
		t.Errorf("expected name mapping for consent_a")
	}
	if _, ok := values[":consent_a"]; !ok {
		t.Errorf("expected :consent_a value")
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./dynamo/... -v`
Expected: FAIL — `dynamo/marshal.go` and `dynamo/base.go` don't exist yet.

- [ ] **Step 4: Write marshal.go (verbatim from either repo — byte-identical)**

`dynamo/marshal.go`:

```go
package dynamo

import (
	"fmt"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// deleteNulls recursively removes null attributes from a DynamoDB attribute
// map, descending into nested maps and list elements.
func deleteNulls(av map[string]types.AttributeValue) {
	for k, val := range av {
		switch v := val.(type) {
		case *types.AttributeValueMemberNULL:
			delete(av, k)
		case *types.AttributeValueMemberM:
			deleteNulls(v.Value)
		case *types.AttributeValueMemberL:
			for _, elem := range v.Value {
				if m, ok := elem.(*types.AttributeValueMemberM); ok {
					deleteNulls(m.Value)
				}
			}
		}
	}
}

// MarshalMapOmitNull marshals v into a DynamoDB attribute map, omitting any
// attribute whose value is null (recursively, including nested maps and list
// elements). This keeps stored items small without changing the API contract —
// reads reconstruct absent attributes as null.
func MarshalMapOmitNull(v any) (map[string]types.AttributeValue, error) {
	av, err := attributevalue.MarshalMap(v)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	deleteNulls(av)
	return av, nil
}
```

- [ ] **Step 5: Write base.go (reconciled superset of dfe/wallet — see Global Constraints for what's excluded)**

`dynamo/base.go`:

```go
// Package dynamo provides the shared DynamoDB persistence primitives used
// across CTech services.
//
// Key design rules:
//   - get_item > query > scan  (no scans in production)
//   - transact_write for atomic multi-item operations
//   - Table names are prefixed by environment: {prefix}_{table}
package dynamo

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Base provides common DynamoDB operations for all repositories.
type Base struct {
	db          *dynamodb.Client
	TableName   string
	tablePrefix string
}

// TableName returns the environment-prefixed physical table name
// ({prefix}_{table}). Exported for call sites outside the repository layer
// that need the physical name without a repository (e.g. a health probe).
func TableName(tablePrefix, table string) string {
	return fmt.Sprintf("%s_%s", tablePrefix, table)
}

// NewBase creates a Base repository with an environment-prefixed table name.
func NewBase(db *dynamodb.Client, tablePrefix, table string) Base {
	return Base{
		db:          db,
		TableName:   TableName(tablePrefix, table),
		tablePrefix: tablePrefix,
	}
}

// NowStr returns the current UTC time as ISO 8601.
func NowStr() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// GetItem fetches a single item by PK (and optional SK).
func (b *Base) GetItem(ctx context.Context, pk string, sk ...string) (map[string]types.AttributeValue, error) {
	key := map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: pk},
	}
	if len(sk) > 0 && sk[0] != "" {
		key["sk"] = &types.AttributeValueMemberS{Value: sk[0]}
	}

	out, err := b.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(b.TableName),
		Key:       key,
	})
	if err != nil {
		return nil, wrapDynamoErr(err)
	}
	if out.Item == nil {
		return nil, nil
	}
	return out.Item, nil
}

// GetItemByRawKey fetches a single item using a caller-supplied key map.
// Use when the sort key is not a standard string "sk" field (e.g. numeric NSU).
func (b *Base) GetItemByRawKey(ctx context.Context, key map[string]types.AttributeValue) (map[string]types.AttributeValue, error) {
	out, err := b.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(b.TableName),
		Key:       key,
	})
	if err != nil {
		return nil, wrapDynamoErr(err)
	}
	if out.Item == nil {
		return nil, nil
	}
	return out.Item, nil
}

// PutItem writes an item to the table.
func (b *Base) PutItem(ctx context.Context, item map[string]types.AttributeValue) error {
	_, err := b.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(b.TableName),
		Item:      item,
	})
	return wrapDynamoErr(err)
}

// buildUpdateExpr builds a combined SET/REMOVE update expression. Nil values
// become REMOVE clauses (clearing the attribute without storing a NULL);
// non-nil values become SET clauses.
func buildUpdateExpr(updates map[string]any) (string, map[string]string, map[string]types.AttributeValue, error) {
	setParts := make([]string, 0, len(updates))
	removeParts := make([]string, 0)
	exprNames := make(map[string]string, len(updates))
	exprValues := make(map[string]types.AttributeValue)

	for attr, val := range updates {
		exprNames["#"+attr] = attr
		if val == nil {
			removeParts = append(removeParts, "#"+attr)
			continue
		}
		av, err := attributevalue.Marshal(val)
		if err != nil {
			return "", nil, nil, err
		}
		setParts = append(setParts, fmt.Sprintf("#%s = :%s", attr, attr))
		exprValues[":"+attr] = av
	}

	clauses := make([]string, 0, 2)
	if len(setParts) > 0 {
		clauses = append(clauses, "SET "+strings.Join(setParts, ", "))
	}
	if len(removeParts) > 0 {
		clauses = append(clauses, "REMOVE "+strings.Join(removeParts, ", "))
	}
	return strings.Join(clauses, " "), exprNames, exprValues, nil
}

// UpdateItem applies a SET/REMOVE expression to an existing item. Nil values
// in updates clear the attribute via REMOVE instead of writing a NULL.
// Returns false if the item does not exist (ConditionCheckFailed).
func (b *Base) UpdateItem(ctx context.Context, pk string, sk *string, updates map[string]any) (bool, error) {
	key := map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: pk},
	}
	if sk != nil {
		key["sk"] = &types.AttributeValueMemberS{Value: *sk}
	}

	expr, exprNames, exprValues, err := buildUpdateExpr(updates)
	if err != nil {
		return false, err
	}

	input := &dynamodb.UpdateItemInput{
		TableName:                aws.String(b.TableName),
		Key:                      key,
		UpdateExpression:         aws.String(expr),
		ExpressionAttributeNames: exprNames,
		ConditionExpression:      aws.String("attribute_exists(pk)"),
	}
	if len(exprValues) > 0 {
		input.ExpressionAttributeValues = exprValues
	}

	_, err = b.db.UpdateItem(ctx, input)
	if err != nil {
		if isConditionFailed(err) {
			return false, nil
		}
		return false, wrapDynamoErr(err)
	}
	return true, nil
}

// UpsertAttrs applies a PARTIAL update, creating the item when absent. It is
// UpdateItem without the attribute_exists guard.
//
// Use it when the row holds several independently written fields (e.g. a
// user row's separately-written consent documents): a whole-row PutItem would
// clobber fields this writer does not own, while UpdateItem would silently
// drop the very first write because no row exists yet.
func (b *Base) UpsertAttrs(ctx context.Context, pk string, sk *string, updates map[string]any) error {
	key := map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: pk},
	}
	if sk != nil {
		key["sk"] = &types.AttributeValueMemberS{Value: *sk}
	}

	expr, exprNames, exprValues, err := buildUpdateExpr(updates)
	if err != nil {
		return err
	}

	input := &dynamodb.UpdateItemInput{
		TableName:                aws.String(b.TableName),
		Key:                      key,
		UpdateExpression:         aws.String(expr),
		ExpressionAttributeNames: exprNames,
	}
	if len(exprValues) > 0 {
		input.ExpressionAttributeValues = exprValues
	}

	_, err = b.db.UpdateItem(ctx, input)
	return wrapDynamoErr(err)
}

// DeleteItem removes an item. Returns false if not found.
func (b *Base) DeleteItem(ctx context.Context, pk string, sk ...string) (bool, error) {
	key := map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: pk},
	}
	if len(sk) > 0 && sk[0] != "" {
		key["sk"] = &types.AttributeValueMemberS{Value: sk[0]}
	}

	_, err := b.db.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName:           aws.String(b.TableName),
		Key:                 key,
		ConditionExpression: aws.String("attribute_exists(pk)"),
	})
	if err != nil {
		if isConditionFailed(err) {
			return false, nil
		}
		return false, wrapDynamoErr(err)
	}
	return true, nil
}

// BuildPutTxItem returns a TransactWriteItem equivalent to PutItem, for composing
// a multi-item transaction via TransactWrite instead of writing immediately.
func (b *Base) BuildPutTxItem(item map[string]types.AttributeValue) types.TransactWriteItem {
	return types.TransactWriteItem{
		Put: &types.Put{
			TableName: aws.String(b.TableName),
			Item:      item,
		},
	}
}

// BuildPutTxItemIfAbsent is like BuildPutTxItem but fails the transaction if
// an item with the same key already exists — used for create-only semantics
// (e.g. person dedup by CPF/CNPJ) instead of the default overwrite-on-put.
func (b *Base) BuildPutTxItemIfAbsent(item map[string]types.AttributeValue) types.TransactWriteItem {
	return types.TransactWriteItem{
		Put: &types.Put{
			TableName:           aws.String(b.TableName),
			Item:                item,
			ConditionExpression: aws.String("attribute_not_exists(pk)"),
		},
	}
}

// BuildUpdateTxItem returns a TransactWriteItem equivalent to UpdateItem, for
// composing a multi-item transaction via TransactWrite instead of writing
// immediately. Same SET/REMOVE semantics and attribute_exists(pk) condition as
// UpdateItem.
func (b *Base) BuildUpdateTxItem(pk string, sk *string, updates map[string]any) (types.TransactWriteItem, error) {
	key := map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: pk},
	}
	if sk != nil {
		key["sk"] = &types.AttributeValueMemberS{Value: *sk}
	}

	expr, exprNames, exprValues, err := buildUpdateExpr(updates)
	if err != nil {
		return types.TransactWriteItem{}, err
	}

	update := &types.Update{
		TableName:                aws.String(b.TableName),
		Key:                      key,
		UpdateExpression:         aws.String(expr),
		ExpressionAttributeNames: exprNames,
		ConditionExpression:      aws.String("attribute_exists(pk)"),
	}
	if len(exprValues) > 0 {
		update.ExpressionAttributeValues = exprValues
	}
	return types.TransactWriteItem{Update: update}, nil
}

// BuildRawUpdateTxItem returns a TransactWriteItem with a caller-supplied update
// and condition expression. Used for balance math that must be relative and
// conditional in one atomic step (e.g. "SET balance = balance - :amt" guarded by
// "balance >= :amt") — semantics buildUpdateExpr's absolute SET cannot express.
func (b *Base) BuildRawUpdateTxItem(pk string, sk *string, updateExpr, condExpr string, names map[string]string, values map[string]types.AttributeValue) types.TransactWriteItem {
	key := map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: pk},
	}
	if sk != nil {
		key["sk"] = &types.AttributeValueMemberS{Value: *sk}
	}
	update := &types.Update{
		TableName:                 aws.String(b.TableName),
		Key:                       key,
		UpdateExpression:          aws.String(updateExpr),
		ExpressionAttributeNames:  names,
		ExpressionAttributeValues: values,
	}
	if condExpr != "" {
		update.ConditionExpression = aws.String(condExpr)
	}
	return types.TransactWriteItem{Update: update}
}

// BuildDeleteTxItem returns a TransactWriteItem equivalent to DeleteItem, for
// composing a multi-item transaction via TransactWrite instead of writing
// immediately.
func (b *Base) BuildDeleteTxItem(pk string, sk ...string) types.TransactWriteItem {
	key := map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: pk},
	}
	if len(sk) > 0 && sk[0] != "" {
		key["sk"] = &types.AttributeValueMemberS{Value: sk[0]}
	}
	return types.TransactWriteItem{
		Delete: &types.Delete{
			TableName:           aws.String(b.TableName),
			Key:                 key,
			ConditionExpression: aws.String("attribute_exists(pk)"),
		},
	}
}

// QueryResult holds paginated query results.
type QueryResult struct {
	Items            []map[string]types.AttributeValue
	LastEvaluatedKey map[string]types.AttributeValue
}

// Query runs a DynamoDB Query with optional SK prefix and pagination.
func (b *Base) Query(ctx context.Context, opts QueryOpts) (*QueryResult, error) {
	opts.defaults()
	input := &dynamodb.QueryInput{
		TableName:                 aws.String(b.TableName),
		KeyConditionExpression:    aws.String(fmt.Sprintf("%s = :pk", opts.PKField)),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": &types.AttributeValueMemberS{Value: opts.PK}},
		ScanIndexForward:          aws.Bool(opts.ScanIndexForward),
		Limit:                     aws.Int32(int32(opts.Limit)),
	}

	if opts.IndexName != "" {
		input.IndexName = aws.String(opts.IndexName)
	}
	if opts.SKPrefix != "" {
		input.KeyConditionExpression = aws.String(
			fmt.Sprintf("%s = :pk AND begins_with(#sk, :sk_prefix)", opts.PKField),
		)
		if input.ExpressionAttributeNames == nil {
			input.ExpressionAttributeNames = make(map[string]string)
		}
		input.ExpressionAttributeNames["#sk"] = opts.SKField
		input.ExpressionAttributeValues[":sk_prefix"] = &types.AttributeValueMemberS{Value: opts.SKPrefix}
	}
	if opts.ExclusiveStartKey != nil {
		input.ExclusiveStartKey = opts.ExclusiveStartKey
	}
	if opts.FilterField != "" {
		input.FilterExpression = aws.String("#filter_field = :filter_value")
		if input.ExpressionAttributeNames == nil {
			input.ExpressionAttributeNames = make(map[string]string)
		}
		input.ExpressionAttributeNames["#filter_field"] = opts.FilterField
		input.ExpressionAttributeValues[":filter_value"] = &types.AttributeValueMemberS{Value: opts.FilterValue}
	}

	out, err := b.db.Query(ctx, input)
	if err != nil {
		return nil, wrapDynamoErr(err)
	}
	return &QueryResult{Items: out.Items, LastEvaluatedKey: out.LastEvaluatedKey}, nil
}

// QueryOpts configures a Query call.
type QueryOpts struct {
	PK                string
	PKField           string // default "pk"
	SKField           string // default "sk"
	SKPrefix          string
	IndexName         string
	ScanIndexForward  bool
	Limit             int
	ExclusiveStartKey map[string]types.AttributeValue
	// FilterField/FilterValue apply a post-key-condition equality filter
	// (DynamoDB FilterExpression) — e.g. narrowing a GSI query whose partition
	// key isn't the org, back down to one org's rows. Both must be set together.
	FilterField string
	FilterValue string
}

func (o *QueryOpts) defaults() {
	if o.PKField == "" {
		o.PKField = "pk"
	}
	if o.SKField == "" {
		o.SKField = "sk"
	}
	if o.Limit == 0 {
		o.Limit = 100
	}
}

// QueryGSI queries a GSI by a single key attribute (equality). Aliases the
// key attribute (#k) so reserved words like "status" are legal index key names.
func (b *Base) QueryGSI(ctx context.Context, indexName, keyName, keyValue string, limit int, startKey map[string]types.AttributeValue) (*QueryResult, error) {
	if limit <= 0 {
		limit = 1
	}
	input := &dynamodb.QueryInput{
		TableName:                 aws.String(b.TableName),
		IndexName:                 aws.String(indexName),
		KeyConditionExpression:    aws.String("#k = :v"),
		ExpressionAttributeNames:  map[string]string{"#k": keyName},
		ExpressionAttributeValues: map[string]types.AttributeValue{":v": &types.AttributeValueMemberS{Value: keyValue}},
		Limit:                     aws.Int32(int32(limit)),
	}
	if startKey != nil {
		input.ExclusiveStartKey = startKey
	}
	out, err := b.db.Query(ctx, input)
	if err != nil {
		return nil, wrapDynamoErr(err)
	}
	return &QueryResult{Items: out.Items, LastEvaluatedKey: out.LastEvaluatedKey}, nil
}

// UpdateItemRaw runs an arbitrary UpdateItem expression.
func (b *Base) UpdateItemRaw(ctx context.Context, input *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
	input.TableName = aws.String(b.TableName)
	out, err := b.db.UpdateItem(ctx, input)
	return out, wrapDynamoErr(err)
}

// TransactWrite executes a DynamoDB transact write with the provided items.
func (b *Base) TransactWrite(ctx context.Context, items []types.TransactWriteItem) error {
	_, err := b.db.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems: items,
	})
	return wrapDynamoErr(err)
}

// AtomicIncrement increments a numeric field and returns the new value.
func (b *Base) AtomicIncrement(ctx context.Context, pk string, sk *string, field string) (int64, error) {
	key := map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: pk},
	}
	if sk != nil {
		key["sk"] = &types.AttributeValueMemberS{Value: *sk}
	}

	out, err := b.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                aws.String(b.TableName),
		Key:                      key,
		UpdateExpression:         aws.String("ADD #f :inc SET updated_at = :now"),
		ExpressionAttributeNames: map[string]string{"#f": field},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":inc": &types.AttributeValueMemberN{Value: "1"},
			":now": &types.AttributeValueMemberS{Value: NowStr()},
		},
		ReturnValues: types.ReturnValueAllNew,
	})
	if err != nil {
		return 0, wrapDynamoErr(err)
	}

	if av, ok := out.Attributes[field]; ok {
		if nv, ok := av.(*types.AttributeValueMemberN); ok {
			n, _ := strconv.ParseInt(nv.Value, 10, 64)
			return n, nil
		}
	}
	return 0, nil
}

// Decode unmarshals DynamoDB attribute values into the target struct.
func Decode[T any](item map[string]types.AttributeValue) (*T, error) {
	var out T
	if err := attributevalue.UnmarshalMap(item, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Encode marshals a value into DynamoDB attribute values, omitting nulls.
func Encode(v any) (map[string]types.AttributeValue, error) {
	return MarshalMapOmitNull(v)
}

func wrapDynamoErr(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("dynamodb: %w", err)
}

func isConditionFailed(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := errors.AsType[*types.ConditionalCheckFailedException](err); ok {
		return true
	}
	return strings.Contains(err.Error(), "ConditionalCheckFailed")
}

func isTransactionCanceled(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := errors.AsType[*types.TransactionCanceledException](err); ok {
		return true
	}
	return strings.Contains(err.Error(), "TransactionCanceledException")
}

// IsConditionFailed reports whether err represents a DynamoDB conditional
// check failure, either from a single-item call or from within a
// TransactWrite (TransactionCanceledException wrapping a condition failure).
// Exported for the services layer to translate into problem.Conflict.
func IsConditionFailed(err error) bool {
	return isConditionFailed(err) || isTransactionCanceled(err)
}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./dynamo/... -v`
Expected: PASS (all `TestMarshalMapOmitNull`, `TestNewBasePrefixesTable`, `TestBuildUpdateExpr_*`, `TestBase_*`
subtests)

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum dynamo/
git commit -m "feat: add dynamo package (marshal + base repository primitives)"
```

---

### Task 4: `problem` package

**Files:**

- Create: `problem/problem.go`
- Test: `problem/problem_test.go`

**Interfaces:**

- Produces: `problem.Problem`, `problem.FieldError`, `problem.New(status int, typ, title, detail string) *Problem`, and
  the 9 generic constructors: `BadRequest`, `Unauthorized`, `Forbidden`, `NotFound`, `Conflict`, `UnprocessableEntity`,
  `Validation`, `TooManyRequests`, `InternalServer`.
- Excludes (per Global Constraints): fiscal-specific (`NoCertificate`, `SefazRejection`) and wallet-specific (
  `InsufficientBalance`, `WalletBusy`, etc.) constructors stay in each consumer's own `problem` package, built on top of
  this one.

- [ ] **Step 1: Write the failing test**

`problem/problem_test.go`:

```go
package problem

import (
	"net/http"
	"testing"
)

func TestNew(t *testing.T) {
	p := New(http.StatusConflict, TypeConflict, "Conflict", "duplicate entry")
	if p.Status != http.StatusConflict {
		t.Errorf("Status = %d, want %d", p.Status, http.StatusConflict)
	}
	if p.Type != TypeConflict {
		t.Errorf("Type = %q, want %q", p.Type, TypeConflict)
	}
	if p.Title != "Conflict" {
		t.Errorf("Title = %q, want %q", p.Title, "Conflict")
	}
	if p.Detail != "duplicate entry" {
		t.Errorf("Detail = %q, want %q", p.Detail, "duplicate entry")
	}
}

func TestBadRequest(t *testing.T) {
	p := BadRequest("bad input")
	if p.Status != http.StatusBadRequest {
		t.Errorf("Status = %d, want %d", p.Status, http.StatusBadRequest)
	}
	if p.Type != TypeBadRequest {
		t.Errorf("Type = %q, want %q", p.Type, TypeBadRequest)
	}
}

func TestValidationCarriesFieldErrors(t *testing.T) {
	p := Validation([]FieldError{
		{Field: "person.cpf", Message: "invalid CPF", Tag: "cpf"},
	})
	if p.Status != http.StatusUnprocessableEntity {
		t.Errorf("Status = %d, want %d", p.Status, http.StatusUnprocessableEntity)
	}
	if len(p.Errors) != 1 || p.Errors[0].Field != "person.cpf" {
		t.Errorf("Errors = %+v, want one FieldError for person.cpf", p.Errors)
	}
}

func TestNotFound(t *testing.T) {
	p := NotFound("organization not found")
	if p.Status != http.StatusNotFound {
		t.Errorf("Status = %d, want %d", p.Status, http.StatusNotFound)
	}
}

func TestInternalServer(t *testing.T) {
	p := InternalServer("unexpected failure")
	if p.Status != http.StatusInternalServerError {
		t.Errorf("Status = %d, want %d", p.Status, http.StatusInternalServerError)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./problem/... -v`
Expected: FAIL — `problem/problem.go` doesn't exist yet.

- [ ] **Step 3: Write problem.go**

`problem/problem.go`:

```go
// Package problem implements RFC 7807 Problem Details, shared across CTech
// services so every service in the platform emits consistent error bodies.
// Service-specific problem types (e.g. fiscal, wallet) live in each consumer's
// own problem package, built on top of the generic constructors here.
package problem

import "net/http"

const (
	TypeBadRequest          = "/problems/bad-request"
	TypeUnauthorized        = "/problems/unauthorized"
	TypeForbidden           = "/problems/forbidden"
	TypeNotFound            = "/problems/not-found"
	TypeConflict            = "/problems/conflict"
	TypeUnprocessableEntity = "/problems/unprocessable-entity"
	TypeValidation          = "/problems/validation-error"
	TypeTooManyRequests     = "/problems/too-many-requests"
	TypeInternalServer      = "/problems/internal-server-error"
)

// FieldError describes a single field-level validation failure.
type FieldError struct {
	Field   string `json:"field"`         // dotted JSON path, e.g. "person.addresses[0].postal_code"
	Message string `json:"message"`       // human-readable message
	Tag     string `json:"tag,omitempty"` // validation rule that failed, e.g. "required", "cnpj"
}

// Problem is an RFC 7807 Problem Details response body. Errors carries field
// failures (only populated for validation problems; omitted otherwise).
// MaxAgeSeconds carries the step-up freshness window on step-up-required
// problems. MinAmount/MaxAmount carry the accepted range on out-of-range
// problems so the UI can state the bounds without hardcoding them. All three
// are optional extension fields — omitted unless a specific problem sets them.
type Problem struct {
	Type          string       `json:"type"`
	Title         string       `json:"title"`
	Status        int          `json:"status"`
	Detail        string       `json:"detail,omitempty"`
	Errors        []FieldError `json:"errors,omitempty"`
	MaxAgeSeconds int          `json:"max_age_seconds,omitempty"`
	MinAmount     int64        `json:"min_amount,omitempty"`
	MaxAmount     int64        `json:"max_amount,omitempty"`
}

func (p *Problem) Error() string {
	if p.Detail != "" {
		return p.Title + ": " + p.Detail
	}
	return p.Title
}

// New builds a Problem with the given status, type URI, title, and detail.
func New(status int, typ, title, detail string) *Problem {
	return &Problem{Type: typ, Title: title, Status: status, Detail: detail}
}

func BadRequest(detail string) *Problem {
	return New(http.StatusBadRequest, TypeBadRequest, "Bad Request", detail)
}

func Unauthorized(detail string) *Problem {
	return New(http.StatusUnauthorized, TypeUnauthorized, "Unauthorized", detail)
}

func Forbidden(detail string) *Problem {
	return New(http.StatusForbidden, TypeForbidden, "Forbidden", detail)
}

func NotFound(detail string) *Problem {
	return New(http.StatusNotFound, TypeNotFound, "Not Found", detail)
}

func Conflict(detail string) *Problem {
	return New(http.StatusConflict, TypeConflict, "Conflict", detail)
}

func UnprocessableEntity(detail string) *Problem {
	return New(http.StatusUnprocessableEntity, TypeUnprocessableEntity, "Unprocessable Entity", detail)
}

// Validation returns a 422 problem carrying the given field-level errors.
// Used by the request-binding layer when a request body fails struct validation.
func Validation(errs []FieldError) *Problem {
	p := New(http.StatusUnprocessableEntity, TypeValidation, "Validation Error", "")
	p.Errors = errs
	return p
}

func TooManyRequests(detail string) *Problem {
	return New(http.StatusTooManyRequests, TypeTooManyRequests, "Too Many Requests", detail)
}

func InternalServer(detail string) *Problem {
	return New(http.StatusInternalServerError, TypeInternalServer, "Internal Server Error", detail)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./problem/... -v`
Expected: PASS (5 tests)

- [ ] **Step 5: Commit**

```bash
git add problem/
git commit -m "feat: add problem package (generic RFC 7807 constructors)"
```

---

### Task 5: `awsconfig` package

**Files:**

- Create: `awsconfig/awsconfig.go`
- Test: `awsconfig/awsconfig_test.go`

**Interfaces:**

- Produces: `awsconfig.Load(ctx context.Context, region string) (aws.Config, error)`,
  `awsconfig.NewDynamoDBClient(cfg aws.Config, endpointOverride string) *dynamodb.Client`.
- Consumes: `github.com/aws/aws-sdk-go-v2/service/dynamodb` (already a dependency from Task 3).

- [ ] **Step 1: Write the failing test**

`awsconfig/awsconfig_test.go`:

```go
package awsconfig

import (
	"context"
	"testing"
)

func TestLoadSetsRegion(t *testing.T) {
	cfg, err := Load(context.Background(), "us-east-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Region != "us-east-1" {
		t.Errorf("Region = %q, want %q", cfg.Region, "us-east-1")
	}
}

func TestNewDynamoDBClientNoOverride(t *testing.T) {
	cfg, err := Load(context.Background(), "us-east-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	client := NewDynamoDBClient(cfg, "")
	if client == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestNewDynamoDBClientWithOverride(t *testing.T) {
	cfg, err := Load(context.Background(), "us-east-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	client := NewDynamoDBClient(cfg, "http://localhost:8000")
	if client == nil {
		t.Fatal("expected non-nil client")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./awsconfig/... -v`
Expected: FAIL — `awsconfig/awsconfig.go` doesn't exist yet.

- [ ] **Step 3: Add aws-sdk-go-v2/config dependency**

```bash
cd /home/artur/Documents/Projects/Ctech/ctech-go-common
go get github.com/aws/aws-sdk-go-v2/config@v1.32.30
```

- [ ] **Step 4: Write awsconfig.go**

`awsconfig/awsconfig.go`:

```go
// Package awsconfig provides the shared AWS SDK v2 config-loading and
// DynamoDB-client bootstrap used across CTech services. Each service still
// owns its own set of AWS clients (S3, SQS, SNS, Lambda, SSM, ...) — only the
// config load and the DynamoDB endpoint-override pattern (for local
// DynamoDB-local development) is common enough to share.
package awsconfig

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

// Load resolves AWS credentials via the standard SDK chain (env vars →
// ~/.aws/credentials → EC2 IMDS → ECS task role) for the given region.
func Load(ctx context.Context, region string) (aws.Config, error) {
	return awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
}

// NewDynamoDBClient builds a DynamoDB client from the given AWS config. A
// non-empty endpointOverride points the client at a local endpoint (e.g.
// DynamoDB-local) instead of the resolved AWS endpoint.
func NewDynamoDBClient(cfg aws.Config, endpointOverride string) *dynamodb.Client {
	var opts []func(*dynamodb.Options)
	if endpointOverride != "" {
		opts = append(opts, func(o *dynamodb.Options) {
			o.BaseEndpoint = aws.String(endpointOverride)
		})
	}
	return dynamodb.NewFromConfig(cfg, opts...)
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./awsconfig/... -v`
Expected: PASS (3 tests)

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum awsconfig/
git commit -m "feat: add awsconfig package (config load + DynamoDB client bootstrap)"
```

---

### Task 6: `ws` package

**Files:**

- Create: `ws/registry.go`
- Create: `ws/memory.go`
- Create: `ws/redis.go`
- Test: `ws/memory_test.go`

**Interfaces:**

- Produces: `ws.Conn` interface, `ws.Registry` interface, `ws.NewMemoryRegistry() *MemoryRegistry`,
  `ws.NewRedisRegistry(client valkey.Client) *RedisRegistry`.
- Consumes: `github.com/valkey-io/valkey-go` (already a dependency from Task 2).
- Note: parameter renamed from `orgPK`/`userID` (cosmetic difference between the two source repos — logic is identical)
  to `key`, a neutral name for the opaque fan-out identifier.
- Note: `RedisRegistry`'s internal fan-out mechanism changes shape from go-redis (`pubsub.Channel()` ranged in a `for`
  loop) to valkey-go (`client.Receive(ctx, cmd, callback)`, blocking with a callback instead of a channel to range
  over) — see Global Constraints and `redis.go` below.

- [ ] **Step 1: Write the failing test**

`ws/memory_test.go` (ported from `ctech-wallet/api/internal/ws/memory_test.go`, `user1`/`user2` are just arbitrary key
values — unaffected by the parameter rename):

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./ws/... -v`
Expected: FAIL — `ws/registry.go`/`ws/memory.go` don't exist yet.

- [ ] **Step 3: Write registry.go**

`ws/registry.go`:

```go
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
```

- [ ] **Step 4: Write memory.go**

`ws/memory.go`:

```go
package ws

import (
	"context"
	"log/slog"
	"sync"
)

// MemoryRegistry is a single-instance connection registry. Use RedisRegistry
// to fan out across multiple API instances.
type MemoryRegistry struct {
	mu    sync.Mutex
	conns map[string]map[string]Conn // key → connID → conn
}

func NewMemoryRegistry() *MemoryRegistry {
	return &MemoryRegistry{conns: make(map[string]map[string]Conn)}
}

func (m *MemoryRegistry) Register(key, connID string, conn Conn) {
	m.mu.Lock()
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
```

- [ ] **Step 5: Write redis.go (valkey-go — callback-based Pub/Sub, not a channel to range over)**

`ws/redis.go`:

```go
package ws

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/valkey-io/valkey-go"
)

const channelPrefix = "ws:"

// RedisRegistry fans out WebSocket messages across all API instances via
// Valkey Pub/Sub. Each instance holds local connections; Valkey is the
// fan-out bus.
type RedisRegistry struct {
	client   valkey.Client
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
// subscription ends cleanly (returns nil), the client is closed manually
// (returns valkey.ErrClosing), ctx is done (returns ctx.Err()), or the
// connection drops (returns a non-nil err) — in the last case, back off and
// resubscribe. If Psubscribe's pattern method turns out to be .Channel()
// instead of .Pattern() (see Global Constraints), that's the only line to fix.
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

		if err == nil || errors.Is(err, valkey.ErrClosing) || ctx.Err() != nil {
			return
		}

		slog.Warn("valkey pubsub closed, reconnecting", "delay", retryDelay, "err", err)
		time.Sleep(retryDelay)
		retryDelay = min(retryDelay*2, 60*time.Second)
	}
}
```

- [ ] **Step 6: Run test to verify it passes**

Run: `go test ./ws/... -v`
Expected: PASS (4 tests)

- [ ] **Step 7: Commit**

```bash
git add ws/
git commit -m "feat: add ws package (connection registry, memory + redis fan-out)"
```

---

### Task 7: Full test suite, README, tag, push

**Files:**

- Modify: `README.md`

**Interfaces:**

- Consumes: all packages from Tasks 2–6.

- [ ] **Step 1: Run the full test suite**

Run: `go build ./... && go vet ./... && go test ./... -race -v`
Expected: PASS across `cache`, `dynamo`, `problem`, `awsconfig`, `ws` — no build or vet errors.

- [ ] **Step 2: Run go mod tidy**

```bash
cd /home/artur/Documents/Projects/Ctech/ctech-go-common
go mod tidy
git diff go.mod go.sum
```

Expected: no unexpected dependency changes (only indirect deps that `go mod tidy` adds/prunes).

- [ ] **Step 3: Finalize README.md**

`README.md`:

```markdown
# ctech-go-common

Shared Go module for the CTech platform. Reconciles internal packages that were independently
duplicated (and drifted) between `ctech-dfe/api` and `ctech-wallet/api`:

| Package     | What it holds                                                            |
|-------------|---------------------------------------------------------------------------|
| `cache`     | `Backend` interface + in-memory and Redis/Valkey implementations         |
| `dynamo`    | DynamoDB persistence primitives (`Base`, `Query`, transact-item builders, `MarshalMapOmitNull`) |
| `problem`   | RFC 7807 Problem Details — generic constructors (`BadRequest`, `NotFound`, `Validation`, ...) |
| `awsconfig` | AWS SDK v2 config load + DynamoDB client bootstrap (with local-endpoint override) |
| `ws`        | WebSocket connection registry, fanned out across instances via Redis Pub/Sub |

## Import path

```

import "gopkg.aoctech.app/api-commons/dynamo"

```

Import as `gopkg.aoctech.app/api-commons`, **not** `github.com/artur-oliveira/ctech-go-common` —
the vanity path is served by [`ctech-vanity`](https://github.com/artur-oliveira/ctech-vanity)'s
`go-import` redirect and is what lets the backing repo move without breaking every consumer's
import path. `ctech-dfe` and `ctech-wallet` will switch their own module paths to
`gopkg.aoctech.app/*` too, in their own follow-up migration plans.

## What's intentionally NOT here

- `CRUDRepository[T]` (org-scoped generic CRUD wrapper) — `ctech-dfe`-specific (multi-tenant);
  `ctech-wallet` has no equivalent. Stays in `ctech-dfe`, built on top of `dynamo.Base`.
- Per-service `Clients` struct (which AWS services to wire up) — `ctech-dfe` and `ctech-wallet`
  use genuinely different service sets (S3/SQS/SNS/Lambda/SecretsManager vs. SSM-only). Only the
  config-load + DynamoDB-client bootstrap (`awsconfig`) is shared.
- Fiscal-specific (`NoCertificate`, `SefazRejection`) and wallet-specific
  (`InsufficientBalance`, `WalletBusy`, ...) `problem` constructors — these live in each
  consumer's own `problem` package, built on the generic constructors here.
- Auth/JWT middleware — `ctech-dfe`, `ctech-wallet`, and `ctech-account` have genuinely different
  trust models (multi-tenant RBAC vs. user+M2M vs. account's own OIDC core); only the underlying
  token validation primitives would ever be shared, and that extraction is out of scope here.

## Development

```bash
go build ./...
go vet ./...
go test ./... -race
```

## License

[Elastic License 2.0 (ELv2)](LICENSE.md) — same license as the other CTech repositories.

```

- [ ] **Step 4: Commit**

```bash
git add README.md go.mod go.sum
git commit -m "docs: finalize README with package overview and scope notes"
```

- [ ] **Step 5: Tag v0.1.0 — CONFIRM WITH USER BEFORE PUSHING**

This pushes to the public remote `artur-oliveira/ctech-go-common` — stop here and confirm before running:

```bash
git tag -a v0.1.0 -m "v0.1.0: cache, dynamo, problem, awsconfig, ws"
git push origin main
git push origin v0.1.0
```

- [ ] **Step 6: Verify go get resolves the vanity path**

Run (from any directory, e.g. `/tmp`):

```bash
mkdir -p /tmp/ctech-go-common-smoketest && cd /tmp/ctech-go-common-smoketest
go mod init smoketest
go get gopkg.aoctech.app/api-commons@v0.1.0
```

Expected: resolves through `gopkg.aoctech.app`'s go-import redirect to
`github.com/artur-oliveira/ctech-go-common`, downloads `v0.1.0`, adds it to `go.mod`/`go.sum`.

---

## Self-Review

**Spec coverage:** Every package flagged in `_analysis/cross-stack-duplication.md`'s "Recommendation" is covered:
`cache` (Task 2), `marshal`/DynamoDB helpers (Task 3), `problem` (Task 4), AWS client wrapper reduced to its genuinely
shared subset (Task 5), Redis pub/sub helper (Task 6). The doc's explicit warning against unifying `middleware/auth.go`
verbatim is honored by *not* including an `auth`/`middleware` package in this module at all.

**Placeholder scan:** No TBD/TODO. Every step has literal, verified file content — either a byte-verbatim port (
confirmed via `diff` exit 0 during research) or an explicit reconciliation of the two existing versions, with the source
diff resolved inline in this plan (e.g. `dynamo.Base` gains `UpsertAttrs`/`BuildRawUpdateTxItem`/`Decode`/`Encode`/the
`QueryGSI` reserved-word fix — all from `ctech-wallet`'s version, since `ctech-dfe`'s was missing them, not the
reverse).

**Type consistency:** `dynamo.NewBase(db *dynamodb.Client, tablePrefix, table string) Base` used identically in Task 3's
implementation and its own test (`TestNewBasePrefixesTable`); this is a deliberate signature change from both source
repos (which took `*config.Config`) — documented in Global Constraints and the Interfaces block, so the (separate,
future) consumer-migration plans know to pass `cfg.TablePrefix` instead of `cfg`. `ws.Registry`'s `key` parameter name
is consistent across `registry.go`, `memory.go`, `redis.go`, and `memory_test.go`.

**Out of scope, by design:** migrating `ctech-dfe`/`ctech-wallet` to consume this module (two separate follow-up plans,
once this one is tagged); `CRUDRepository[T]`; per-service AWS `Clients` structs; auth middleware unification.
