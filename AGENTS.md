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
  `QueryOpts` filters are typed pairs, never raw expressions: `FilterField`/`FilterValue` (equality) and
  `FilterContainsField`/`FilterContainsValue` (`contains(#f, :v)`, for list membership such as
  `persons.roles` holding `"driver"`). Both pairs set → ANDed by `buildFilterExpr`. The filtered attribute
  must be projected into the queried index. DynamoDB applies `Limit` to the items it *evaluates*, before
  the `FilterExpression`, so one call can return an empty page while matches sit further down the
  partition. Since v1.16.0 a filtered `Query` keeps calling (following `LastEvaluatedKey`, each call
  evaluating up to `Limit` items) until `Limit` matches are collected, the partition ends, or
  `QueryOpts.MaxPages` calls are spent (default `DefaultFilteredQueryMaxPages` = 10). When the last call
  over-read, the cursor is the key of the last *returned* item (never the over-read page's raw
  `LastEvaluatedKey`, which would skip matches). At the page cap the page may be short — even empty —
  with a cursor, so callers must still treat an absent cursor, not a short page, as end-of-list.
  Unfiltered queries are one call, unchanged. `QueryRaw` is not wrapped: a caller that passes its own
  `FilterExpression` with a `Limit` owns the paging loop.
  `TransactWrite` `:433` — **requires the `dynamodb:TransactWriteItems` IAM permission**.
  `AtomicIncrement` `:441`, `Decode[T]` `:474`, `Encode` `:483`. `MarshalMapOmitNull` `dynamo/marshal.go:33`.
  **Transaction failures are classified by cancellation reason, not by "it was cancelled"**:
  `IsConditionFailed` (single-item `ConditionalCheckFailedException`, or a `TransactionCanceledException`
  carrying a `ConditionalCheckFailed` reason) is the only one that is a verdict about the caller's own
  condition; `IsTransactionConflict` (a concurrent transaction on the same item) and
  `IsTransactionThrottled` (`ThrottlingError`/`ProvisionedThroughputExceeded`) mean nothing was written
  and the same write should be retried — the AWS SDK retries neither. Until 2026-09-17
  `IsConditionFailed` answered true for *any* cancelled transaction, so a conflict impersonated a lost
  optimistic-concurrency race; `ctech-poker` builds its table-commit recovery on that distinction and
  froze a hand mid-runout because of it (its
  `docs/specs/2026-09-17-frozen-table-runout-and-sitout-fold.md`). An unclassifiable cancellation now
  answers false for all three rather than defaulting to "condition failed".
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
  Revocation: `WithRevocation`, `Revoke`, `Unrevoke`, `CheckRevoked`, `RevocationTTL`, `ErrTokenRevoked`, `ErrRevocationUnavailable`,
  `VerifyClaimsStrict` in `jwtverify/revocation.go` / `jwtverify/verifier.go` (entries in Valkey DB 0).
- `erasure` — LGPD account-deletion participant contract. `Message`/`Encode`/`Decode` `erasure/message.go`;
  `Store` (`Apply`, `Blocked`, `OrgErased`, `Clear`) over `{prefix}_erasure_state` `erasure/state.go`
  (pure transition `apply`: issued_at ordering, erased terminal); `Consumer` `erasure/consumer.go`
  (deletes a message only after purge + tombstone + ack); `AckClient` `erasure/ack.go`.
- `oauth2client` — cached `client_credentials` token fetcher. `TokenManager` `oauth2client/client.go:21`;
  `New` `:34`, `Get` `:40` (refreshes 30s before `expires_in` `:74`).
- `accountorgs` — M2M client for ctech-account's `GET /v1.0/internal/organizations/{org}/members/{user}`
  (scope `MemberScope`) and `GET /v1.0/internal/users/{user}/organizations` (scope `ListScope`), each on its own
  `oauth2client.TokenManager` so a missing list grant never breaks membership checks. `New` returns nil on an
  incomplete `Config` and a nil `*Client` errors on every call (never permission). `Membership` returns a
  `Membership{Member, Role, Kind}`; a refusal is the zero value with a nil error; every non-200 (403 = our
  credential, 404 = route missing; ctech-account answers unknown orgs with 200 `member:false`) and every decode
  failure is an error. `Organizations` returns `[]Organization` (`People`/`PendingInvitations` pointers, absent
  = unknown); 8 KiB body cap per membership, 256 KiB per list. Kinds are raw; `IsOrganizationKind("")` is true.
  Caching of answers stays in each service (its TTL is a product decision). Used by ctech-billing and ctech-dfe.
- `observability` — context-aware structured logging plus `observability/fiber` Request-ID middleware and HTTP error
  boundary. No OpenTelemetry/exporter dependency; consumers own domain classification and safe attributes.
- `problem` — RFC 7807/9457. `Problem` `problem/problem.go`, `FieldError`, type constants, constructors
  `BadRequest` … `InternalServer`, `Validation`. Structured recovery guidance: `WithNextAction(action, seconds)`
  sets the `next_action`/`retry_after_seconds` extension members (`NextActionRetry`/`Wait`/`Reauthenticate`/
  `ContactSupport`); `TooManyRequestsAfter(detail, seconds)` is the rate-limiter shorthand. Both fields are omitted
  unless set, so every existing consumer's bodies are byte-identical until it opts in.
- `ws` — WebSocket registry fanned out via Valkey Pub/Sub. `Registry` iface `ws/registry.go:22`,
  `Conn` `:17`; `RedisRegistry` `ws/redis.go:28` (`NewRedisRegistry` `:42`, `Start` `:72`,
  `Broadcast` `:97`, `listen` `:115` auto-resubscribe), `MemoryRegistry` `ws/memory.go:11` (single-instance).
- `awsconfig` — `Load` `awsconfig/awsconfig.go:18`, `NewDynamoDBClient` `:25` (local-endpoint override).
- `drain` — graceful-shutdown coordinator for long-lived connections. `Tracker` `drain/tracker.go:16` (zero value
  ready to use); `Register` `:23` (rejects after drain starts), `Unregister` `:40`, `Draining` `:47`, `Drain` `:55`
  (idempotent; calls every registered `CloseFunc` and joins their errors). Callers own wiring this to their own
  SIGTERM/signal handler — this package has no signal-handling code itself.
- `patch` — PATCH-body field `Optional[T]` (`patch/optional.go`): absent / null / value. An explicit JSON null
  clears an optional field; absent keeps it. `Of`, `Null`, `UnmarshalJSON`, `Present`, `IsNull`, `Get`, `Ptr`.
  Decode-only (no `MarshalJSON`: encodes as `{}`); duplicate key → last wins; decode into a fresh struct. Since
  `v1.14.0`, extracted from ctech-billing `api/internal/patch` with identical API.
- `ratelimit` — transport-agnostic, Valkey-backed rate limiter core (extracted from ctech-account's middleware).
  `Limiter` `ratelimit/ratelimit.go:41` (`Counter`, `Prefix`, `Max`, `Window`, `FailClosed`); `Take` `:59` (throughput
  guard, atomic incr+decide); brute-force guard via `CheckFailures`/`RecordFailure` in `ratelimit/counter.go`.
  `FailClosed` degrades a counter error to `Unavailable` (503), never to unbounded allow, on auth surfaces.

## Intentionally NOT here

See `README.md` "What's intentionally NOT here": `CRUDRepository[T]` (dfe-only), per-service `Clients`
struct, fiscal/wallet `problem` constructors, and auth/JWT **middleware** (the trust models differ — only the validation
primitive `jwtverify` is shared).

## Consumer version skew

The module is source-distributed via git tags (no auto-bump). Consumers currently pin **different**
versions — confirm before relying on a symbol added after a given tag:

| Consumer                        | api-commons pin |
|---------------------------------|-----------------|
| `ctech-account/api`             | `v1.9.1`        |
| `ctech-dfe/api`                 | `v1.9.1`        |
| `ctech-wallet/api`              | `v1.9.1`        |
| `ctech-wallet/pix-gateway`      | `v1.7.2` (lagging) |
| `ctech-poker/api`               | `v1.9.2`        |
| `ctech-billing/api`             | `v1.8.0`        |

To add a symbol used by a lagging consumer, either bump that consumer's pin or keep the new symbol out of the shared
contract until all are upgraded.

## Release

Semver git tag only (`README.md` "Deploy"). No `go build` publish step; the proxy fetches source from the tagged commit.

## Mandatory Documentation Policy

**Every code change MUST be documented.**

There are NO exceptions.

Any modification affecting behavior, architecture, APIs, integrations, configuration, deployment, security, business rules, or developer workflow MUST include the corresponding documentation update in the same change.
