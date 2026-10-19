# AGENTS.md — ctech-go-common (module `gopkg.aoctech.app/api-commons`)

## CTech Standards (mandatory)

Before writing, changing or reviewing any code or infra here, load the `ctech-dev-standards` skill (plugin
`ctech-skills`, repo github.com/artur-oliveira/ctech-skills). It is the single source of truth for backend,
frontend and infra (patterns, resilience, scale, cost). If anything in this file contradicts it, the skill
wins and this file must be fixed. Install on a new machine:
`/plugin marketplace add artur-oliveira/ctech-skills` then `/plugin install ctech-skills@ctech-skills`.

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
  Unfiltered queries are one call, unchanged. `QueryRaw` is not wrapped: a hand-built filtered query with a
  `Limit` whose result is a page goes through `QueryRawFiltered(ctx, input, maxPages)`, which applies the
  same loop and cursor contract (single call, like `QueryRaw`, without a filter/`Limit` or with
  `Select: COUNT`).
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
  60s. Parsed keys are also served from process memory for 5 min (`keyMemoryTTL`, `WithKeyMemoryTTL`): a request
  touches neither Valkey nor the provider, concurrent cold loads share one fetch, and a provider outage keeps
  serving the keys already held (stale-if-error).
  Revocation: `WithRevocation`, `Revoke`, `Unrevoke`, `CheckRevoked`, `RevocationTTL`, `ErrTokenRevoked`, `ErrRevocationUnavailable`,
  `VerifyClaimsStrict` in `jwtverify/revocation.go` / `jwtverify/verifier.go` (entries in Valkey DB 0). `VerifyClaims`
  remembers each user's answer in process for 10 s (`WithRevocationMemoryTTL`); `VerifyClaimsStrict` always reads the list.
- `deadline` — Fiber v3 middleware giving every request a time budget (`deadline.New(timeout, skip...)`): `c.Context()` is
  `context.Background()` otherwise, so DynamoDB/Valkey/HTTP calls outlive the client. WebSocket upgrades are never bounded; `skip`
  names other long routes. A propagated `context.DeadlineExceeded` becomes 504.
- `resilient` — the HTTP client for calling another service: `resilient.Client(timeout, Config{})` is an `http.Client` over `NewTransport()`
  (64 idle connections per host instead of net/http's 2, HTTP/2, bounded dial and TLS) with a per-host, in-process circuit breaker
  (`Wrap`): 5 failures in a row (transport error, 502/503/504; not 4xx, 500 or the caller cancelling) open it for 10 s (doubling on a
  failed probe, up to 60 s) and a call answers at once with `*OpenError` (`errors.Is(err, resilient.ErrCircuitOpen)`); then ONE probe
  decides. Used by `jwtverify` (JWKS), `accountorgs`, `oauthclients`; `oauth2client` does not retry an open circuit.
- `ttlcache` — small in-process TTL cache (`New[V](ttl, max)`, `Get`/`Set`/`Delete`) for answers re-read on every
  request; the TTL is the staleness the caller accepts. Used by `jwtverify` revocation and `erasure.Store.Blocked`.
- `erasure` — LGPD account-deletion participant contract. `Store.Blocked` is remembered 10 s per user in process (`WithBlockedMemoryTTL`); a write through the same Store invalidates it. `Consumer.WithVisibilityHeartbeat(extend)` keeps the message being handled invisible while a long purge or export runs (every extend/3 it sets the visibility timeout to `extend`, up to 3 h; needs `sqs:ChangeMessageVisibility`; off by default). `Message`/`Encode`/`Decode` `erasure/message.go`;
  `Store` (`Apply`, `Blocked`, `OrgErased`, `Clear`) over `{prefix}_erasure_state` `erasure/state.go`
  (pure transition `apply`: issued_at ordering, erased terminal); `Consumer` `erasure/consumer.go`
  (deletes a message only after purge + tombstone + ack); `AckClient` `erasure/ack.go`.
- `dataexport` — personal-data export participant contract. `Message`/`Encode`/`Decode`/`IsExport`
  `dataexport/message.go` (type `user.export`, shares the erasure queue); `Handler` + `PartFunc`
  `dataexport/handler.go` (build part, PUT to the presigned url, ack; presigned URLs never logged). A part too big for memory uses
  `NewStreamHandler` + `PartStreamFunc` (writes the zip to an `io.Writer` page by page; the handler spools it to a temp file
  and PUTs it with the exact Content-Length a presigned S3 PUT requires; 1 GiB cap; the spool file is always removed);
  `AckClient` `dataexport/ack.go`. `BeginItems`/`Items.Add`/`End` `dataexport/items.go` write one zip entry as
  `{"items":[...],"truncated":bool}` an item at a time (the streaming counterpart of marshalling a slice).
  Hook: `erasure.Consumer.WithExporter(handler.Handle)`.
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
- `oauthclients` — M2M client for ctech-account's `GET /v1.0/internal/oauth-clients/{client_id}` (scope
  `ReadScope` = `internal:account:oauth-client-read`): `Get` returns `OAuthClient{ClientID, OwnerUserID,
  AllowedScopes, Active}` (never the secret; `AllowedScopes` never nil). An unknown client is `ErrNotFound`, told
  apart from a missing route by the 404's problem `type` (`…/problems/not-found`; a missing route is a
  problem+json 404 of type `request-error`); every other non-200, a bad body
  and an answer about another id are errors. Empty or >128-character ids are `ErrNotFound` without a request.
  `New` returns nil on an incomplete `Config` and a nil `*Client` errors (never permission). No answer caching.
  Since `v1.17.0`; first consumer ctech-billing (console credentials).
- `byok` — provider-neutral "bring your own key" seam (an organization's own bank/PSP account reached with its own
  credentials). `Provider` `byok/provider.go` (credentials per call: plaintext `Values` or a sealed `Ref` the caller's
  outbound Lambda opens); civil `Date` (`ParseDate` `byok/types.go:16`, JSON `"YYYY-MM-DD"`); money in centavos;
  outbound envelope `Request`/`Response`, `Op`, `AllOps`, `GroupOf` `byok/envelope.go:46` (`read` / `teardown` /
  `collect` / `sandbox`, the allowlist classes a gate keys on); sentinel errors with wire codes `ErrorCode`
  `byok/errors.go:37` / `ErrorFromCode` (unknown error = `"internal"`). `byok/byoktest.New` `byok/byoktest/fake.go:30`
  is a deterministic in-memory `Provider` (`Pay`, `AddLines`, `SetBalanceAt`, `FailNext`, `Calls`). Since `v1.18.0`;
  first consumer ctech-billing (BYOK phase 1).
- `inter` — Banco Inter's `byok.Provider`. `NewClient` `inter/client.go:103` caches one `*http.Transport` per
  client-certificate fingerprint (dropped after 30 min idle); `FetchToken` `:153` (client_credentials over mTLS, never
  cached here: callers own the token lifecycle); `InspectPair` `:58` (`ErrCertificateInvalid` / `ErrKeyMismatch` /
  `ErrCertificateExpired`, no network). Calls: `PutCobv`/`GetCobv`/`PatchCobvAmount`/`RemoveCobv`, devolução, Pix
  webhook get/put/delete, callbacks list/retry (≤ 50 txids), sandbox `PaySandboxCobv` (`inter/pix_calls.go`);
  `ExtratoCompleto` (pagination, never scroll) and `Saldo` (`inter/banking_calls.go`). Bacen types in `inter/pix`
  (types only). Statuses map to `byok` errors; a provider body never reaches an error text (only `title` and
  violation field names). `NewProvider` `inter/provider.go:19`; `Connect` `:41` asks the full phase-1 scope set and
  falls back to `extrato.read` (statement-only); `ParseCallback` `inter/callback.go:13` keeps references only.
  `inter/qr.PNG` renders a BR Code. Reads no SSM and no environment. Since `v1.18.0`.
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
  `Bucket` `ratelimit/bucket.go` is the in-process per-key token bucket (`NewBucket(rate, burst, maxKeys)`, `Allow(key)`
  → ok + retryAfter; bounded memory, a nil/zero one is off): the per-tenant fairness guard, no Valkey round trip, counted per
  process. `ratelimit/fiberlimit.New(bucket, key, deny)` is its Fiber middleware (sets Retry-After; the service supplies the 429 body).
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
| `ctech-billing/api`             | `v1.18.0` (`byok`, `inter`) |

To add a symbol used by a lagging consumer, either bump that consumer's pin or keep the new symbol out of the shared
contract until all are upgraded.

## Release

Semver git tag only (`README.md` "Deploy"). No `go build` publish step; the proxy fetches source from the tagged commit.

## Mandatory Documentation Policy

**Every code change MUST be documented.**

There are NO exceptions.

Any modification affecting behavior, architecture, APIs, integrations, configuration, deployment, security, business rules, or developer workflow MUST include the corresponding documentation update in the same change.
