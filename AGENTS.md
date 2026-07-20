# AGENTS.md — ctech-go-common (module `gopkg.aoctech.app/api-commons`)

**Reuse me, don't fork.** This is the shared Go module for the CTech platform. Before copying a DynamoDB helper, cache
wrapper, lock, JWT verifier, OAuth2 client, RFC-7807 `problem`, WebSocket registry, or AWS-config bootstrap into your
service, import it from here.

## Import path (canonical)

```
import "gopkg.aoctech.app/api-commons/dynamo"
```

The vanity path `gopkg.aoctech.app/api-commons` is served by `ctech-vanity`'s `go-import` redirect
(`ctech-vanity/src/index.ts:9`). Do **not** import `github.com/artur-oliveira/ctech-go-common`
directly — that path is the backing repo and may move.

## What lives here (anchored to file:line)

- `dynamo` — single-table persistence primitives. `Base` struct `dynamo/base.go:25`; `NewBase` `:39`;
  `GetItem` `:53`, `GetItemByRawKey` `:76`, `PutItem` `:91`, `UpdateItem` `:135` (returns false on
  `attribute_exists` miss), `UpsertAttrs` `:176` (no exists-guard), `DeleteItem` `:204`. Transaction builders:
  `BuildPutTxItem` `:228`, `BuildPutTxItemIfAbsent` `:240`, `BuildUpdateTxItem`
  `:254`, `BuildRawUpdateTxItem` `:284` (relative math w/ condition), `BuildDeleteTxItem` `:307`.
  `Query` `:330` + `QueryOpts` `:373`, `QueryGSI` `:403`, `UpdateItemRaw` `:426`.
  `TransactWrite` `:433` — **requires the `dynamodb:TransactWriteItems` IAM permission**.
  `AtomicIncrement` `:441`, `Decode[T]` `:474`, `Encode` `:483`, `IsConditionFailed` `:518`
  (maps `TransactionCanceledException` too). `MarshalMapOmitNull` `dynamo/marshal.go:33`.
- `cache` — `Backend` interface `cache/cache.go:7` (Get/Set/Delete/DeletePrefix/Ping). Valkey impl
  `RedisBackend` `cache/redis.go:13` (`NewRedisBackend` `:17`, `DeletePrefix` `:62` escapes glob metachars); in-memory
  impl `MemoryBackend` `cache/memory.go:17` (single-instance only).
- `lock` — CAS acquire/renew/release. `Locker` `lock/lock.go:45`; `New` `:57` picks Valkey store when given a
  `*cache.RedisBackend`, else in-memory; `Acquire` `:70` (returns release func),
  `AcquireOrdered` `:103` (lexicographic sort → deadlock-free, all-or-nothing), `Renew` `:133`,
  `StartHeartbeat` `:155` + `DefaultHeartbeatInterval` `:32`. TTL is a required constructor arg.
- `jwtverify` — RS256 access-token validation against ctech-account JWKS. `Verifier` `jwtverify/verifier.go:72`;
  `NewVerifier` `:82`, `Ping` `:89` (health check), `VerifyClaims` `:101`, `Claims` `:48`
  (`Scopes` `:58`, `HasScope` `:61`). JWKS cached under `ctech:jwks` TTL 1h `:27-28`; unknown-kid refresh throttled to
  60s `:32`, `:150`.
- `oauth2client` — cached `client_credentials` token fetcher. `TokenManager` `oauth2client/client.go:21`;
  `New` `:34`, `Get` `:40` (refreshes 30s before `expires_in` `:74`).
- `problem` — RFC 7807. `Problem` `problem/problem.go:34`, `FieldError` `:22`, type constants `:9-19`, constructors
  `BadRequest` `:57` … `InternalServer` `:93`, `Validation` `:83`.
- `ws` — WebSocket registry fanned out via Valkey Pub/Sub. `Registry` iface `ws/registry.go:22`,
  `Conn` `:17`; `RedisRegistry` `ws/redis.go:28` (`NewRedisRegistry` `:42`, `Start` `:72`,
  `Broadcast` `:97`, `listen` `:115` auto-resubscribe), `MemoryRegistry` `ws/memory.go:11` (single-instance).
- `awsconfig` — `Load` `awsconfig/awsconfig.go:18`, `NewDynamoDBClient` `:25` (local-endpoint override).

## Intentionally NOT here

See `README.md` "What's intentionally NOT here": `CRUDRepository[T]` (dfe-only), per-service `Clients`
struct, fiscal/wallet `problem` constructors, and auth/JWT **middleware** (the trust models differ — only the validation
primitive `jwtverify` is shared).

## Consumer version skew (divergence — flag, do not "fix" unilaterally)

The module is source-distributed via git tags (no auto-bump). Consumers currently pin **different**
versions — confirm before relying on a symbol added after a given tag:

| Consumer                                               | api-commons pin      | Note                                                 |
|--------------------------------------------------------|----------------------|------------------------------------------------------|
| `ctech-account/api`                                    | `v1.0.2`             | **B23** — behind                                     |
| `ctech-wallet/pix-gateway`                             | `v1.1.0` // indirect | **B16** — indirect, older than `wallet/api` `v1.2.0` |
| `ctech-dfe/api`, `ctech-wallet/api`, `ctech-poker/api` | `v1.2.0`             | current                                              |

To add a symbol used by a lagging consumer, either bump that consumer's pin or keep the new symbol out of the shared
contract until all are upgraded.

## Release

Semver git tag only (`README.md` "Deploy"). No `go build` publish step; the proxy fetches source from the tagged commit.
