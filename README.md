# ctech-go-common

This is a shared foundation library consumed by every CTech Go service — treat every change here (defaults,
behavior, error semantics, cache/lock backends) as a cross-repo contract change, not a local tweak. Confirmed
consumers today: `ctech-account/api`, `ctech-dfe/api`, `ctech-wallet/api`, `ctech-wallet/pix-gateway`, `ctech-poker/api`,
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
| `lock`         | CAS acquire/renew/release lock (Valkey + in-memory), for advisory locks and long-held leases    |
| `observability` | Structured slog helpers, Request-ID context and Fiber correlation/error-boundary integration   |
| `email`        | SESv2 transport: one HTML send and a raw send for threaded mail. Templates stay in each service  |
| `alerts`       | Operator alerts published to an SNS topic — the cheap replacement for per-metric CloudWatch alarms |
| `drain`        | `Tracker` coordinates graceful shutdown of long-lived connections (e.g. websockets): register/unregister a per-connection `CloseFunc`, `Drain` asks every live connection to reconnect elsewhere. Callers wire it to their own SIGTERM handler |
| `ratelimit`    | Transport-agnostic, Valkey-backed rate limiter core (throughput-guard `Take` and brute-force-guard `CheckFailures`/`RecordFailure` shapes); each API wraps a `Limiter` in its own thin HTTP middleware |
| `erasure`      | Participant side of the LGPD account-deletion saga: message contract (`Message`, `Encode`/`Decode`), `{prefix}_erasure_state` lock/tombstone `Store`, SQS `Consumer`, `AckClient`, eligibility/blocker types. Orchestration lives in ctech-account |

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

## Account erasure (LGPD deletion)

Contract: `ctech-account/docs/specs/2026-10-06-account-deletion-saga-protocol.md`.

Each participant service (wallet, dfe, billing, poker):

1. Creates a `{prefix}_erasure_state` DynamoDB table (partition key `pk` String, TTL attribute `ttl`)
   and an SQS queue + DLQ subscribed to ctech-account's `{env}-account-user-erasure` SNS topic
   (filter policy on `services`, scope `MessageBody`; raw delivery recommended, the envelope is also accepted).
   Queue visibility timeout ≥ 2× the slowest purge.
2. Builds `erasure.NewStore(dynamoClient, tablePrefix, retention)` and calls `store.Blocked(ctx, sub)` on
   every user write path (refuse when true), and `store.OrgErased` on org-scoped async work.
3. Implements a `PurgeFunc` (idempotent, resumable, re-checks eligibility, returns `done`/`blocked`) and
   runs `erasure.NewConsumer(sqsClient, queueURL, "<service>", store, purge, erasure.NewAckClient(...)).Run(ctx)`
   in a background goroutine. The ack client uses an `oauth2client.TokenManager` with scope `account:erasure:ack`.
4. Serves `GET /internal/erasure/eligibility/{sub}` returning `erasure.NewEligibility(blockers...)`.

Messages arrive out of order: lock/unlock are ordered by `issued_at`, and `erased` is terminal.

### JWT revocation

`Verifier.WithRevocation(backend)` rejects tokens of a locked user (`ErrTokenRevoked`). ctech-account writes the
entries with `jwtverify.Revoke` / `jwtverify.Unrevoke`. The entries live in **Valkey DB 0** (the shared base URL
`/ctech/{env}/valkey/url`, no DB suffix): services whose main cache uses another logical DB (wallet DB 2,
billing DB 3) must pass a second `cache.RedisBackend` built on the base URL. `VerifyClaims` fails open if Valkey is
unreachable; money-moving routes use `VerifyClaimsStrict`, which fails closed with `ErrRevocationUnavailable`.

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

**Consumer version skew (verify before using a newly released symbol):** consumers currently pin different tags —
`ctech-account/api` and `ctech-dfe/api` at `v1.9.1`, `ctech-wallet/api` at `v1.9.1`, `ctech-wallet/pix-gateway` at
`v1.7.2` (lagging), `ctech-poker/api` at `v1.9.2`, and `ctech-billing/api` at `v1.8.0`. The module is git-tag
source-distributed and has no automatic consumer bump, so confirm each consumer's actual pin in its `go.mod` before
relying on a symbol added after an older tag — `pix-gateway` in particular is several releases behind its sibling
`ctech-wallet/api`.



### JWT verification policy

The shared verifier accepts only RS256 access tokens with token_use=access. Published JWKs must be RSA and, when metadata is present, declare sig and RS256. Production consumers must provide issuer and audience.

## Acknowledged realtime publishing (v1.12.0)

`ws.Publisher` is an optional interface implemented by both registries. `Publish(ctx, key, payload)` returns transport acceptance or an error. Redis publishing does not fall back locally on failure; durable outbox consumers retry instead. Existing `Registry.Broadcast` retains its local fallback and signature. Acceptance does not acknowledge every socket: clients still require sequence checks and snapshot recovery. Memory publishing is for a single process and rejects cancellation/draining.
