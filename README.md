# ctech-go-common

This is a shared foundation library consumed by every CTech Go service — treat every change here (defaults,
behavior, error semantics, cache/lock backends) as a cross-repo contract change, not a local tweak. Confirmed
consumers today: `ctech-account/api`, `ctech-dfe/api`, `ctech-wallet/api`, `ctech-poker/api`,
and `ctech-billing/api` (see "Consumer version skew" below for exact pins).

Shared Go module for the CTech platform. Reconciles internal packages that were independently duplicated (and drifted)
between `ctech-dfe/api` and `ctech-wallet/api`:

| Package        | What it holds                                                                                   |
|----------------|-------------------------------------------------------------------------------------------------|
| `cache`        | `Backend` interface + in-memory and Valkey implementations                                      |
| `dynamo`       | DynamoDB persistence primitives (`Base`, `Query`, transact-item builders, `MarshalMapOmitNull`, sampled `SetCapacityRecorder`) |
| `problem`      | RFC 7807 Problem Details — generic constructors (`BadRequest`, `NotFound`, `Validation`, ...)   |
| `awsconfig`    | AWS SDK v2 config load + DynamoDB client bootstrap (with local-endpoint override)               |
| `ws`           | WebSocket connection registry, fanned out across instances via Valkey Pub/Sub                   |
| `oauth2client` | Cached OAuth2 client_credentials token fetcher, shared across M2M callers                       |
| `accountorgs`  | M2M client for ctech-account's organization facts: `Membership` (role + kind of a person in an organization) and `Organizations` (a person's workspaces), one token per scope, fail-closed (nil client, non-200, bad body are errors; "not a member" is `Membership{}`, nil). Replaces the copies in ctech-billing and ctech-dfe |
| `oauthclients` | M2M reader of one ctech-account OAuth client (`owner_user_id`, `allowed_scopes`, `active`, never the secret) for products that admit a person's client as a credential; fail-closed, unknown client is `ErrNotFound` |
| `lock`         | CAS acquire/renew/release lock (Valkey + in-memory), for advisory locks and long-held leases    |
| `observability` | Structured slog helpers, Request-ID context and Fiber correlation/error-boundary integration   |
| `email`        | SESv2 transport: one HTML send and a raw send for threaded mail. Templates stay in each service  |
| `alerts`       | Operator alerts published to an SNS topic — the cheap replacement for per-metric CloudWatch alarms |
| `drain`        | `Tracker` coordinates graceful shutdown of long-lived connections (e.g. websockets): register/unregister a per-connection `CloseFunc`, `Drain` asks every live connection to reconnect elsewhere. Callers wire it to their own SIGTERM handler |
| `ratelimit`    | Transport-agnostic, Valkey-backed rate limiter core (throughput-guard `Take` and brute-force-guard `CheckFailures`/`RecordFailure` shapes); each API wraps a `Limiter` in its own thin HTTP middleware |
| `erasure`      | Participant side of the LGPD account-deletion saga: message contract (`Message`, `Encode`/`Decode`), `{prefix}_erasure_state` lock/tombstone `Store`, SQS `Consumer`, `AckClient`, eligibility/blocker types. Orchestration lives in ctech-account |
| `dataexport`   | Participant side of the personal-data export (ctech-account spec 2026-10-17): `Message` (`user.export`, presigned PUT url, no personal data), `Handler` (builds the service part via `PartFunc`, uploads it, acks), `AckClient`. Rides the erasure queue: plug `Handler.Handle` into `erasure.Consumer.WithExporter`. Orchestration lives in ctech-account |
| `patch`        | `Optional[T]` for HTTP PATCH bodies: absent / null / value per field. An explicit JSON null clears an optional field; absent keeps it (see "Partial updates") |
| `byok`         | Provider-neutral "bring your own key" seam: `Provider` (charges with due date, refunds, statement, balance, webhooks), money in centavos, civil `Date`, the outbound-Lambda envelope (`Request`, `Response`, `Op`, `GroupOf`), wire error codes. `byok/byoktest` is an in-memory `Provider` for tests |
| `inter`        | Banco Inter's `byok.Provider`: mTLS client with one transport per certificate fingerprint, `FetchToken`, Pix `cobv`/devolução/webhook/callbacks, Banking extrato completo (pagination, never scroll) and saldo, Bacen RFC 7807 errors, `qr.PNG`. Credentials per call; no SSM, no env, no token cache |

## Import path

```
import "gopkg.aoctech.app/api-commons/dynamo"
```

Import as `gopkg.aoctech.app/api-commons`, **not** `github.com/artur-oliveira/ctech-go-common` — the vanity path is
served by [`ctech-vanity`](https://github.com/artur-oliveira/ctech-vanity)'s
`go-import` redirect and is what lets the backing repo move without breaking every consumer's import path. `ctech-dfe`
and `ctech-wallet` will switch their own module paths to
`gopkg.aoctech.app/*` too, in their own follow-up migration plans.

## What's intentionally NOT here

- `CRUDRepository[T]` (org-scoped generic CRUD wrapper) — `ctech-dfe`-specific (multi-tenant);
  `ctech-wallet` has no equivalent. Stays in `ctech-dfe`, built on top of `dynamo.Base`.
- Per-service `Clients` struct (which AWS services to wire up) — `ctech-dfe` and `ctech-wallet`
  use genuinely different service sets (S3/SQS/SNS/Lambda/SecretsManager vs. SSM-only). Only the config-load +
  DynamoDB-client bootstrap (`awsconfig`) is shared.
- Fiscal-specific (`NoCertificate`, `SefazRejection`) and wallet-specific (`InsufficientBalance`, `WalletBusy`, ...)
  `problem` constructors — these live in each consumer's own `problem` package, built on the generic constructors here.
- Auth/JWT middleware — `ctech-dfe`, `ctech-wallet`, and `ctech-account` have genuinely different trust models
  (multi-tenant RBAC vs. user+M2M vs. account's own OIDC core); only the underlying token validation primitives would
  ever be shared, and that extraction is out of scope here.

## Error observability

`observability` is the platform logging contract without OpenTelemetry or an exporter. `Error` and `Warn` attach
`request_id` from `context.Context`; `LogHTTPError` records every HTTP rejection at `WARN` and every server failure at
`ERROR`. `observability/fiber.RequestID` assigns or preserves `X-Request-ID`, echoes it in the response and propagates
it into the Go context. Consumers keep domain-specific error classification and safe attributes locally.

A problem can also carry **structured recovery guidance**: `Problem.WithNextAction(action, retryAfterSeconds)` sets
the RFC 9457 extension members `next_action` (one of `NextActionRetry`, `NextActionWait`,
`NextActionReauthenticate`, `NextActionContactSupport`) and `retry_after_seconds`, so a client can recover on its own
instead of pattern-matching on `detail`. Both are omitted unless set, and `retryAfterSeconds` 0 means "unknown" and
stays omitted — a fabricated window is worse than none, because clients back off on whatever number is there. Only
set it where the service actually knows the answer; `TooManyRequestsAfter(detail, seconds)` does it for the one case
that always knows (a rate limiter and its own window).

Internal causes can be attached to a shared RFC 7807 problem with `Problem.WithCause`. `cause` is unexported and is
never serialized; Fiber-facing consumer wrappers log it before writing the safe public body. Logs must not contain
credentials, tokens, cookies, request bodies, email addresses, tax identifiers or other unnecessary PII.

## Partial updates (PATCH bodies)

`patch.Optional[T]` is one field of a PATCH body. **An explicit JSON null clears an optional field; absent keeps it;
a value replaces it.** A `*T` cannot express that, because absent and null both decode to nil.

```go
type updateBill struct {
	Description patch.Optional[string] `json:"description"`
	DueDate     patch.Optional[Date]   `json:"due_date"`
}

var body updateBill // always a fresh value per request
if err := json.Unmarshal(raw, &body); err != nil { /* 400: reject the whole body */ }
switch {
case !body.Description.Present(): // absent: keep what is stored
case body.Description.IsNull():   // null: clear (or 422 if the field is required)
default:
	v, _ := body.Description.Get() // a value: replace; "" and 0 are values, not absence
}
```

API: `Of(v)`, `Null[T]()`, `Present`, `IsNull`, `Get` (value, ok), `Ptr` (copy, nil when absent or null),
`UnmarshalJSON`. The zero value is absent. `T` decodes by its own rules, so a wrong type is a decode error. A duplicate
key: the last occurrence wins. The string `"null"` is a value. `Optional` is decode-only: it has no `MarshalJSON`, so it
encodes as `{}` and `omitempty` does not omit it. `json.Unmarshal` leaves unnamed keys untouched, so a reused struct
keeps the previous body's state. Extracted from `ctech-billing` (`api/internal/patch`) in `v1.14.0`.

## Account erasure (LGPD deletion)

Contract: `ctech-account/docs/specs/2026-10-06-account-deletion-saga-protocol.md`.

Each participant service (wallet, dfe, billing, poker):

1. Creates a `{prefix}_erasure_state` DynamoDB table (partition key `pk` String, TTL attribute `ttl`)
   and an SQS queue + DLQ subscribed to ctech-account's `{env}-account-user-erasure` SNS topic
   (filter policy on `services`, scope `MessageAttributes`, the `services` message attribute; raw delivery recommended, the envelope is also accepted).
   Queue visibility timeout ≥ 2× the slowest purge.
2. Builds `erasure.NewStore(dynamoClient, tablePrefix, retention)` and calls `store.Blocked(ctx, sub)` on
   every user write path (refuse when true), and `store.OrgErased` on org-scoped async work.
3. Implements a `PurgeFunc` (idempotent, resumable, re-checks eligibility, returns `done`/`blocked`) and
   runs `erasure.NewConsumer(sqsClient, queueURL, "<service>", store, purge, erasure.NewAckClient(...)).Run(ctx)`
   in a background goroutine. The ack client uses an `oauth2client.TokenManager` with scope `internal:account:erasure-ack`.
4. Serves `GET /internal/erasure/eligibility/{sub}` returning `erasure.NewEligibility(blockers...)`.

`Message.Workspaces` (`{id, kind}`, additive since v1.19.0) is a superset of `Organizations`; use `m.WorkspaceIDs()` for the
ids and `m.KindOf(id)` for the kind (an id only in `Organizations` is an organization).

`Consumer.OnLock(fn)` (v1.20.0) calls `fn(ctx, sub)` when the consumer moves a sub to locked: on `user.locked`, or on the
synthetic lock before a `user.erase`. It does not fire for a redelivery, a stale message, or an erase after a delivered
lock. Use it to cut what is already open (websockets) on the replica that received the message and fan the cut out to the
others; it has no error return, so keep the connect-time `Store.Blocked` check as the safety net. `Store.ApplyChanged` is
`Apply` plus "did the state change".

Messages arrive out of order: lock/unlock are ordered by `issued_at`, and `erased` is terminal.

`user.relinked` (`TypeRelinked`, v1.28.0) is sent by ctech-account when a person who left **one** service (`scope: service`)
signs in to it again. It needs `Scope: service` and exactly one target service. The consumer calls `Store.Relink(sub, issued_at)` (ordered like lock/unlock, so a late duplicate after the person left again is ignored) and
sends no ack; on a record that is not erased it is a no-op. **Bump to v1.28.0 before ctech-account enables relink for your
service**: an older consumer rejects the unknown type and the message ends in the DLQ.

### JWT revocation

`Verifier.WithRevocation(backend)` rejects tokens of a locked user (`ErrTokenRevoked`). ctech-account writes the
entries with `jwtverify.Revoke` / `jwtverify.Unrevoke`. The entries live in **Valkey DB 0** (the shared base URL
`/ctech/{env}/valkey/url`, no DB suffix): services whose main cache uses another logical DB (wallet DB 2,
billing DB 3) must pass a second `cache.RedisBackend` built on the base URL. `VerifyClaims` fails open if Valkey is
unreachable; money-moving routes use `VerifyClaimsStrict`, which fails closed with `ErrRevocationUnavailable`.
Services that verify tokens themselves (ctech-account) call `jwtverify.CheckRevoked(ctx, backend, sub, iat)` directly.

### Rule: every new feature ships its erasure path

ctech-account runs an LGPD deletion saga across the whole CTech family (specs and review in
`ctech-account/docs/specs/2026-10-06-account-deletion-*.md` and `docs/2026-10-13-account-deletion-plan-review.md`).
Every service is a participant. The participant plans were written once and went stale as features landed, so this is a rule:

**A feature that stores or derives data tied to a person or an organization (a table, an index, a key prefix, a bucket
prefix, a cache key, a queue or job row, a third-party registration, an audit field) is not done until its deletion
flow exists and is tested.** In the same change:

1. Add its row to the service's data inventory (erase, anonymize or retain, with legal basis and expiry; retention needs a
   reason, not a convenience).
2. Implement the erase or anonymize step in the service's purge, idempotent and resumable (re-runnable from scratch).
3. Add or update the eligibility blockers if it can hold money, debt, open work or ownership.
4. Make every async path (webhook, job, sweep, queue consumer) drop work for a locked or erased `sub` or organization.
5. Test it: seed the new data, run the purge twice, assert it is gone or anonymized, and that a late write for an erased
   subject is rejected. The purge's table-classification test must fail for an unclassified table; keep it exhaustive.
6. Revoke or delete what lives at third parties (e.g. webhooks or credentials registered at a bank).
7. Update the participant plan or spec, and note the cross-service impact (who else must act when this is erased).

A pull request that adds person-linked data without this is incomplete, whatever else passes.

## Development

```bash
go build ./...
go vet ./...
go test ./...
```

## Deploy

There is no build/publish step — Go modules are source-distributed. A release is just a semver git tag; `go get`/the
module proxy fetches source directly from the tagged commit via the VCS, and consumers compile it themselves.

On every release:

1. Land all changes on `main` — CI (`.github/workflows/ci.yml`) must be green.
2. Decide the version bump per [semver](https://semver.org/): `MAJOR` for breaking API changes (removed/renamed exported
   symbol, changed function signature), `MINOR` for backward-compatible additions, `PATCH` for fixes that don't change
   any exported API.
3. Tag and push:
   ```bash
   git tag -a vX.Y.Z -m "vX.Y.Z: <one-line summary>"
   git push origin main
   git push origin vX.Y.Z
   ```
4. Create the GitHub Release (changelog/visibility only — not required for `go get` to work):
   ```bash
   gh release create vX.Y.Z --title vX.Y.Z --generate-notes
   ```
5. Smoke-test the vanity import path resolves the new tag:
   ```bash
   cd /tmp && mkdir smoketest && cd smoketest && go mod init smoketest
   go get gopkg.aoctech.app/api-commons@vX.Y.Z
   ```
6. Bump the dependency in consumers (`ctech-dfe`, `ctech-wallet`) on their own schedule — this module has no
   auto-bump/auto-deploy hook into either repo.

## License

[Elastic License 2.0 (ELv2)](LICENSE.md) — same license as the other CTech repositories.

## Audited API surface (file:line)

The full, anchored API is in [`AGENTS.md`](AGENTS.md). Headline exports:

| Package        | Key symbols (file:line)                                                                                                                                                                                 |
|----------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `dynamo`       | `Base` `dynamo/base.go:25`, `TransactWrite` `:433` (needs `dynamodb:TransactWriteItems`), `Query` `:330`, `UpsertAttrs` `:176`, `IsConditionFailed` / `IsTransactionConflict` / `IsTransactionThrottled` `dynamo/base.go:846+`, `MarshalMapOmitNull` `dynamo/marshal.go:33` |
| `cache`        | `Backend` `cache/cache.go:7`, `RedisBackend` `cache/redis.go:13`, `MemoryBackend` `cache/memory.go:17`                                                                                                  |
| `lock`         | `Locker` `lock/lock.go:45`, `AcquireOrdered` `:103` (deadlock-free), `StartHeartbeat` `:155`                                                                                                            |
| `jwtverify`    | `Verifier` `jwtverify/verifier.go:72`, `VerifyClaims` `:101`, `Claims` `:48`                                                                                                                            |
| `oauth2client` | `TokenManager` `oauth2client/client.go:21`, `Get` `:40` (refreshes 30s early)                                                                                                                           |
| `problem`      | `Problem` `problem/problem.go`, constructors `BadRequest` … `InternalServer`, `TooManyRequestsAfter`, `WithNextAction`                                                                                  |
| `ws`           | `Registry` `ws/registry.go:22`, `RedisRegistry` `ws/redis.go:28`, `MemoryRegistry` `ws/memory.go:11`                                                                                                    |
| `awsconfig`    | `Load` `awsconfig/awsconfig.go:18`, `NewDynamoDBClient` `:25`                                                                                                                                           |

**Consumer version skew (verify before using a newly released symbol):** on 2026-10-19 the pins were
`ctech-account/api` `v1.29.1`, `ctech-billing/api` `v1.37.0`, `ctech-dfe/api`, `ctech-poker/api` and `ctech-wallet/api` `v1.36.0`. Latest tag: `v1.37.0`. The module is git-tag source-distributed with no automatic consumer bump, so confirm
each consumer's actual pin in its `go.mod` before relying on a symbol added after an older tag.

### JWT verification policy

The shared verifier accepts only RS256 access tokens with token_use=access. Published JWKs must be RSA and, when metadata is present, declare sig and RS256. Production consumers must provide issuer and audience.

## Acknowledged realtime publishing (v1.12.0)

`ws.Publisher` is an optional interface implemented by both registries. `Publish(ctx, key, payload)` returns transport acceptance or an error. Redis publishing does not fall back locally on failure; durable outbox consumers retry instead. Existing `Registry.Broadcast` retains its local fallback and signature. Acceptance does not acknowledge every socket: clients still require sequence checks and snapshot recovery. Memory publishing is for a single process and rejects cancellation/draining.
