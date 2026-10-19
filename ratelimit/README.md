# ratelimit

Shared rate limiting for CTech APIs: one policy (key, limit/window, fail mode, headers), a generic lockout, a trusted
client-IP extractor, and an in-process token bucket. Everything is additive; `Limiter.Take/CheckFailures/RecordFailure`,
`Bucket`, `Counter`, `ValkeyCounter`, `MemoryCounter` and `fiberlimit.New` are unchanged.

Core (`ratelimit`) has no HTTP framework dependency. The Fiber middleware lives in `ratelimit/fiberlimit`.

## When to use what

| Need | Use |
| --- | --- |
| Fleet-wide cap (per IP, user, tenant, composite), Valkey | `Limiter` + `fiberlimit.NewPolicy` |
| Brute-force guard (only failures count) | same, `Count: fiberlimit.CountFailures` |
| "N failures then blocked for a while" on an account/identity | `Lockout` |
| Per-tenant cap with zero Valkey cost, per-process | `Bucket` + `fiberlimit.New` |
| Real client IP behind a load balancer | `IPExtractor` / `fiberlimit.IPKey` |

## API

- `Limiter{Counter, Prefix, Max, Window, FailMode}`: `Take`, `CheckFailures`, `RecordFailure` (existing) plus
  `Decide` / `DecideFailures`, which return a `Decision{Outcome, Limit, Remaining, Reset}` with `RetryAfterSeconds()`.
- `FailMode`: `FailOpen` (default) or `FailClosed`. The old `FailClosed bool` field still works.
- `Key(parts...)`: composite identity `a:b`; returns `""` if any part is empty (unresolved identity is not limited,
  it never collapses anonymous callers into one bucket).
- `TTLCounter`, `Store`: optional counter capabilities (`TTL`, `Delete`) implemented by `ValkeyCounter` and
  `MemoryCounter`. A custom `Counter` without `TTL` still works; `Reset` falls back to the full `Window`.
- `Lockout{Store, Prefix, Threshold, Window, BlockFor, FailMode}`: `Check`, `Fail`, `Reset`, `Unblock`.
- `IPExtractor` (`NewIPExtractor(trustedProxies...)`, `ClientIP(remoteAddr, xffLines...)`).
- `fiberlimit.NewPolicy(PolicyConfig{Limiter, Key, Count, Deny, Unavailable})`, `SkipCount(c)`, `IPKey(ex)`.

## Fail-open vs fail-closed (decide per route, explicitly)

- **FailOpen** (zero value): backend error means the request passes and a warning is logged. For throughput caps
  where availability beats abuse protection (game actions, lists, reads).
- **FailClosed**: backend error means `Unavailable`, and the middleware answers `503` problem
  (`/problems/service-unavailable`, `next_action: retry`). For login, token, password, MFA, payout and money routes,
  where a silent limiter is an unbounded brute-force or fraud hole.
- Lockout follows the same rule on `Check`. `Fail`/`Reset` errors never block an already-produced response.
- Missing identity (`Key` returns `""`) is not limited in either mode.

## Responses

- Allowed: `RateLimit-Limit`, `RateLimit-Remaining`, `RateLimit-Reset` (seconds, IETF draft headers).
- Denied: `429`, the same headers plus `Retry-After`, body `problem.TooManyRequestsAfter` as
  `application/problem+json`. Reset/Retry-After come from the counter TTL, rounded up, minimum 1.
- `PolicyConfig.Deny` / `Unavailable` replace the body when a service keeps its own problem type; headers are set first.

## Trusted client IP

`X-Forwarded-For` is only read when the TCP peer is a trusted proxy, then walked right to left to the first address that
is not a trusted proxy. Anything the client wrote to the left is ignored. No trusted proxies configured means the peer
address. Invalid proxy entries fail construction. Fiber's own `c.IP()` is not used by `fiberlimit.IPKey`.

## Example

```go
ex, _ := ratelimit.NewIPExtractor(cfg.TrustedProxyCIDRs...)
counter := ratelimit.NewValkeyCounter(valkeyClient)

login := fiberlimit.NewPolicy(fiberlimit.PolicyConfig{
    Limiter: &ratelimit.Limiter{Counter: counter, Prefix: "login", Max: 5, Window: 15 * time.Minute, FailMode: ratelimit.FailClosed},
    Key:     fiberlimit.IPKey(ex),
    Count:   fiberlimit.CountFailures,
})
perUser := fiberlimit.NewPolicy(fiberlimit.PolicyConfig{
    Limiter: &ratelimit.Limiter{Counter: counter, Prefix: "user", Max: 100, Window: time.Minute},
    Key:     func(c fiber.Ctx) string { return ratelimit.Key(tenantID(c), userID(c)) },
})

lo := &ratelimit.Lockout{Store: counter, Prefix: "acct", Threshold: 5, Window: 15 * time.Minute, BlockFor: 15 * time.Minute, FailMode: ratelimit.FailClosed}
out, _, _ := lo.Check(ctx, email) // Deny: answer like a wrong password
```

## Extracted from, and how each product migrates

- **ctech-poker** `api/internal/api/v1/ratelimit.go` (`RateLimiter`, `rateLimit`, `ipKey`, `playerKey`, `AllowFailOpen`):
  replace with `Limiter` (FailOpen) + `NewPolicy`; keys via `ratelimit.Key`. WebSocket loops (no `c.Next()`) call
  `Take` and treat error as allow. Behavioural difference: poker used one Lua script (INCR + TTL repair);
  `ValkeyCounter.Incr` is `SET NX EX` then `INCR` (TTL is seeded once and never lost, same fix for the #45 race).
  Poker's in-memory dev backend becomes `MemoryCounter`.
- **ctech-account** `middleware/ratelimit.go`, `lockout.go`: `RateLimitConfig` maps to `Limiter` + `PolicyConfig`
  (`CountOnlyFailures` -> `CountFailures`, `CountOnlySuccesses` -> `CountSuccesses`, `SkipRateLimitCount` ->
  `fiberlimit.SkipCount`, `FailClosed` -> `FailMode`). `AccountLockoutMiddleware` becomes a thin wrapper over `Lockout`
  (its identifier extraction is a stub today and stays account-side). Account keeps its `apierror` type through
  `Deny`/`Unavailable` if it does not adopt `problem` yet.
- **ctech-billing** already uses `Bucket`, `fiberlimit.New` and `Counter` (BYOK limits); nothing breaks. Optional: use
  `Limiter.Decide` in `byokrpc/limits.go` to drop its hand-rolled `take`.
- **ctech-wallet, ctech-dfe**: no limiter today; add `NewPolicy` on auth and money routes (FailClosed) and the rest (FailOpen).

## Not covered

Sliding-window or GCRA algorithms, per-route config loading, and metrics (policy: no custom metrics). The Valkey `TTL`,
`Delete` paths are covered by the in-memory implementation only; there is no Valkey integration test in this repo.
