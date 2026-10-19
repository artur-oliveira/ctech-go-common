# authguard

Fiber v3 authorization middlewares shared by every CTech API, plus a small in-process cache for auth data.
Import: `gopkg.aoctech.app/api-commons/authguard`. It does **not** verify tokens: that stays in
`jwtverify`. The service's auth middleware verifies the token and calls `authguard.SetClaims(c, claims)`;
the guards below run after it and read the claims through `authguard.GetClaims(c)` (single accessor,
Fiber locals key `claims`). A service with its own locals key adds one line to its auth middleware.

## When to use

Any route that must check a scope, a service-to-service caller, MFA freshness, the OAuth client, or
re-check the revocation list. Do not copy these into a product.

## API

| Symbol | Behaviour |
|---|---|
| `RequireScope(s)` / `RequireAnyScope(s...)` / `RequireAllScopes(s...)` | 403 + `WWW-Authenticate: Bearer error="insufficient_scope", scope="..."`. Empty list denies. |
| `RequireInternalScope(s)` | Only `client_credentials` tokens (empty `sid`) holding `s`; a user/API-key token is 403 even with the scope. |
| `RequireRecentMFA(maxAge)` | Reads `last_mfa_at`; stale/absent: 403 `/problems/step-up-required` with `max_age_seconds`. `maxAge <= 0` = `DefaultStepUpMaxAge` (5 min). |
| `RequireClientID(ids...)` | `azp` must be one of `ids`; else 403. |
| `RequireUnrevoked(backend, policy)` | Live read of the revocation list (`jwtverify.CheckRevoked`). |
| `RequireUnrevokedByMethod(backend)` | Same, policy picked from the verb (`PolicyForMethod`). |
| `NewCache[V](loader, CacheOptions)` | `Get(ctx,key)`, `Invalidate(key)`, `Forget(key)`, `OnInvalidate(fn)`. |
| `SetClaims`, `GetClaims`, `LocalsClaims` | Claims access. |

No claims on the request: every guard answers 401 `unauthorized` (never a panic, never a pass).

## Fail-open / fail-closed decisions

- **Revocation, `PolicyReadOpen`**: list unreachable means the request passes (logged as a warning). A readable
  revoked entry is still 401. Reads must survive a Valkey outage; the token lifetime bounds the exposure.
- **Revocation, `PolicyWriteClosed`**: list unreachable means 503 `/problems/service-unavailable`
  (`next_action: retry`). Use it explicitly on money, consent and any mutation. `RequireUnrevokedByMethod` maps
  GET/HEAD/OPTIONS to read-open and **every other verb to write-closed**.
- A `nil` backend disables the check (route tests only; production config must require the backend).
- **Scopes/MFA/client**: pure claim checks, always closed (empty policy denies).
- **Cache**: loader errors are never cached and never turned into an allow; the caller decides what a load error
  means for its route (for an auth decision: deny/503).

## Cache

Read-through, per process, for data re-read on every request (M2M credential, organization membership,
account block). Concurrent misses share one loader call (singleflight, implemented inline on top of
`ttlcache`; no new dependency). The load runs detached from the request context with `LoadTimeout`
(default 2 s), so one cancelled caller does not fail the rest. An `Invalidate` during an in-flight load
discards that load's (stale) result. Staleness is bounded by `TTL` (default 30 s). Multi-instance
invalidation: register `OnInvalidate(func(key){ publish(key) })` and call `Forget(key)` on the receiving side
(`Forget` does not run hooks, so there is no echo loop).

## Example

```go
app.Use(func(c fiber.Ctx) error { // service auth middleware
	cl, err := verifier.VerifyClaims(c.Context(), bearer(c))
	if err != nil { return unauthorized(c) }
	authguard.SetClaims(c, cl)
	return c.Next()
})
api.Use(authguard.RequireUnrevokedByMethod(revocationBackend))
api.Get("/v1.0/balances", authguard.RequireScope("wallet:balances:read"), listBalances)
api.Post("/v1.0/withdrawals",
	authguard.RequireScope("wallet:withdrawals:write"),
	authguard.RequireRecentMFA(authguard.DefaultStepUpMaxAge),
	authguard.RequireUnrevoked(revocationBackend, authguard.PolicyWriteClosed),
	createWithdrawal)
internal.Post("/credit", authguard.RequireInternalScope("internal:wallet:credit"), credit)

blocks := authguard.NewCache(loadBlock, authguard.CacheOptions{TTL: 10 * time.Second})
blocked, err := blocks.Get(c.Context(), userID)
```

## Extracted from

- ctech-account `api/internal/middleware`: `RequireScope`, `RequireInternalScope`, `RequireClientID`, `stepup.go`.
- ctech-wallet `api/internal/middleware`: `scope.go` (`RequireScope` M2M variant), `stepup.go`, `revocation.go`
  (`RequireUnrevoked`, fail-closed 503), `claims.go`.
- ctech-billing `middleware/scope.go` (`RequireM2MScope`).

Unified differences: account's `RequireScope` is a user-or-M2M scope check (= `RequireScope` here), while
wallet/billing's `RequireScope`/`RequireM2MScope` is internal-only (= `RequireInternalScope` here).

## Migration per product

- **ctech-account**: replace `RequireScope`, `RequireInternalScope`, `RequireClientID` in `auth.go` and
  `stepup.go` (+ `stepup_test.go`) with authguard; `RequireAuth` keeps calling `SetClaims`. `apierror.StepUpRequired`
  stays for non-middleware callers. Account keeps its own `RequireAuth` (it signs its own tokens).
- **ctech-wallet**: delete `middleware/stepup.go`, `middleware/revocation.go`; in `scope.go` swap the internal
  `RequireScope` for `RequireInternalScope`; `claims.go` `GetClaims` delegates to `authguard.GetClaims` (set
  `LocalsClaims`, already `"claims"`). `RequireKYC`, `RequireUser` and `AllowsUserScope` stay local (domain-specific).
- **ctech-billing**: `RequireM2MScope` becomes `RequireInternalScope`; add `RequireUnrevoked` on write routes.
- **ctech-poker / ctech-dfe**: use the guards in place of local scope checks; poker's money routes use
  `PolicyWriteClosed`.

Problem bodies are the common `problem` package shape (`type` is relative, e.g. `/problems/step-up-required`),
so a product that prefixes its own problem base URL may see a different `type` string until it migrates.
Products' Portuguese `detail` texts are replaced by English ones (wire `type` and `status` are what clients branch on).
