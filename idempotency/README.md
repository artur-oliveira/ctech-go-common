# idempotency

Fiber v3 middleware that makes mutating routes safe to retry. A client sends `Idempotency-Key`; the first request
runs, any retry gets the first answer back (`Idempotent-Replay: true`) instead of running the operation again.

Use it on every POST/PUT/PATCH/DELETE that creates or changes state (charges, transfers, orders, fiscal documents).
Apply it once at the group/app level, not per handler: the route where it is forgotten is the one that charges twice.

Not a replacement for a guard written **in the same DynamoDB transaction as the state change** (wallet's
`IDEM#key` item next to the ledger entry): that one makes the effect itself exactly-once even when the HTTP answer
is lost. This middleware replays the HTTP answer and blocks concurrent duplicates; keep the transactional guard
where the effect is money and use both.

## Contract

| Situation | Result |
|---|---|
| Method not in `Methods` (default POST, PUT, PATCH, DELETE) | passes through untouched |
| No key, `Required: false` | handler runs, unprotected |
| No key, `Required: true` | 400 `/problems/idempotency-key-required` |
| Key empty, over 128 chars or not visible ASCII | 400 `/problems/idempotency-key-invalid` |
| Scope unresolved | 500 `/problems/idempotency-scope-unresolved` (a wiring bug, never guessed) |
| New key | claimed with one conditional write, handler runs |
| Same key, same method+path+query+body, answer stored | stored status/body/content type, header `Idempotent-Replay: true` |
| Same key, different method/path/query/body | 409 `/problems/idempotency-conflict` |
| Same key, first request still running | **409** `/problems/idempotency-in-progress`, `Retry-After: 1`, `next_action: retry` |
| Store unreachable | `FailOpen: false` (default): 503 `/problems/idempotency-unavailable` + `Retry-After`; `FailOpen: true`: handler runs unprotected, logged |

In-progress is 409 (not 425): 425 Too Early is defined for TLS early data, and 409 is what billing already ships.
Both clients see "retry shortly": once the first finishes, the retry replays its answer.

Problems are written as `application/problem+json` by the middleware itself (it does not need the service's error
handler), using the `problem` package types.

### What is recorded

Recorded and replayed: **2xx** and **deterministic 4xx** (400, 404, 422, ...). Not recorded, the claim is released
so a retry runs: 5xx, handler errors (the app error handler answers later, status unknown), 401/403 (credentials
change), 408, 409, 423, 425, 429 (depend on time/load/state), 1xx/3xx, and answers over `MaxBodyBytes` (default
256 KiB, below DynamoDB's 400 KB item limit; the answer is still served).

If the answer cannot be stored after the handler ran, the response is still returned and the claim stays until its
lease ends: a retry meanwhile gets 409 in-progress rather than running the operation twice.

### Time

- `TTL` (default 24 h, never negative): how long an answer is replayable. Always set on the row.
- `Lease` (default 2 min): how long a claim without an answer blocks the key; a process that died mid-request stops
  blocking after it. Keep it above the request deadline.
- `StoreTimeout` (default 2 s) bounds every store call; completion/release use `context.WithoutCancel`, so a client
  that disconnected does not leave the claim dangling.
- An entry past its expiry counts as free **in the claim condition itself**: DynamoDB TTL deletion is lazy (up to
  ~48 h), so relying on it would replay answers beyond `TTL`.

### Fail-open vs fail-closed

Default is **closed**: if the store cannot be reached the request is refused (503, retryable), because running a
charge without protection is worse than a retry. Set `FailOpen: true` only for routes where a duplicate is harmless
(idempotent by nature, or low value). Mixed policies: build one handler per policy.

## Key scope

`Config.Scope` returns the namespace a key lives in: an M2M credential's organization, a tenant, a user. Two scopes
never share keys. Include the mode if one id spans modes (`org123#live`). `ScopeFromLocals("key")` reads a string put
in `c.Locals` by the auth middleware. Run the middleware **after** authentication.

## DynamoDB table

One table (name via `dynamo.NewBase(db, prefix, "idempotency")`), on-demand, **TTL enabled on attribute `ttl`**.

| attribute | type | content |
|---|---|---|
| `pk` (hash) | S | `IDEM#<scope>` |
| `sk` (range) | S | `KEY#<key>` |
| `ttl` | N | epoch seconds; claim: now+Lease, answer: now+TTL |
| `request_hash` | S | sha256 hex of method, path, query, body (NUL separated) |
| `status` | N | `0` while claimed, HTTP status once answered |
| `content_type` | S | stored answer's Content-Type |
| `response` | B | stored answer body |
| `created_at` | S | RFC 3339 |

Claim is `PutItem` with `attribute_not_exists(pk) OR #ttl < :now` and `ReturnValuesOnConditionCheckFailure=ALL_OLD`:
one write, and a failure returns the existing row (replay / conflict / in-progress) without a second read. Required
IAM: `dynamodb:PutItem`, `dynamodb:DeleteItem` on the table. The request body is hashed, never stored; only the
answer is.

## Usage

```go
store := idempotency.NewDynamoStore(dynamo.NewBase(db, cfg.TablePrefix, "idempotency"))

mutating := idempotency.New(idempotency.Config{
    Store:    store,
    Scope:    func(c fiber.Ctx) (string, bool) { cr := auth.Credential(c); if cr == nil { return "", false }; return cr.OrgID, true },
    Required: true,
})
v1.Use(authMiddleware, mutating) // GETs pass through

// a route where the key is optional: second handler, different policy
optional := idempotency.New(idempotency.Config{Store: store, Scope: scope, FailOpen: true})
```

Tests: `&idempotency.MemoryStore{Now: clock}` (single process only; not for production).
Integration tests for the DynamoDB store run against DynamoDB Local with `-tags integration`
(`docker-compose.test.yml`, `DYNAMODB_ENDPOINT`).

## Extracted from / migration

Extracted from `ctech-billing/api/internal/middleware/idempotency.go` and
`repositories/idempotency.go` (claim, hash, release, replay). Differences unified:

- claim condition also treats an expired **answer** as free (billing only freed lapsed claims);
- key limit 128 (billing 255), visible ASCII only; the 409 in-progress has its own problem type instead of a code;
- store behind an interface; the scope is a function instead of three hardcoded variants (credential, space,
  session); non-final 4xx (401/403/408/409/423/425/429) are not recorded, other 4xx are (billing released all 4xx).

| Product | Migration |
|---|---|
| billing | replace `middleware/idempotency.go` (`Idempotency`, `SpaceIdempotency`, `SessionIdempotency`) with `idempotency.New` + three `ScopeFunc`s (credential org+livemode, space owner+livemode, `USER#sub`); replace `repositories/idempotency.go` with `NewDynamoStore` (same `pk/sk/ttl` idea; row attributes differ: `idem_key`/`response` string become `request_hash`/`response` B, so use a fresh table or let old rows expire in 24 h). `SessionIdempotency`'s GET skip is the default `Methods`. |
| wallet | keep the in-transaction `IDEM#key` guard for ledger writes; add the middleware on the HTTP group for answer replay and concurrent-duplicate refusal. |
| poker | uses idempotency ad hoc in handlers (`reactionpurchase`, `reports`, settlements); adopt the middleware on those groups and drop the handler checks (not reviewed in detail). |
| account, dfe | none today: add `idempotency.New` on their mutating groups with a new table. |
