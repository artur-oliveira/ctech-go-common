# health

Fiber v3 health endpoints in the draft-inadarei-api-health-check shape (`application/health+json`), extracted
from the copies in ctech-billing, wallet, poker, dfe and account (`cmd/api/health.go`).

## When to use

Every Fiber API. Mount two routes:

- **`/health` (liveness)**: no dependencies, always 200 while the process is up. This is what the load balancer
  probes. Never point a balancer at the report: with auto-heal, a dependency outage would replace the whole fleet
  at once (billing's note).
- **`/health-check` (report)**: runs the registered checks. For people and monitoring.

## API

```go
h := health.New(health.Config{ServiceID: "ctech-x", Description: "...", Version: "/v1", ReleaseID: rel})
h.Add(
    health.DynamoDB(dbClient, table, true),          // Primary: failure -> 503
    health.Cache(cacheBackend.Ping, false),          // secondary: failure -> warn / 207
    health.JWKS(verifier.Ping, false),
    health.CPU(), health.Memory(),                   // never fail
)
app.Get("/v1/health", h.Liveness())
app.Get("/v1/health-check", h.Report())
```

Also exported: `Check` (custom checks: `Name`, `Type`, `Primary`, `Probe`), `PingCheck`, `Observation`,
`Handler.Run` (report without HTTP), `Report`/`Entry`, status constants.

## Decisions (fail-open / fail-closed)

- Each check runs concurrently under its own timeout (`DefaultTimeout` = 2s, `Config.Timeout`).
- **Only a `Primary` check can produce 503.** Any other probe error becomes `warn` -> HTTP 207, so a secondary
  dependency can never pull an instance out of rotation. A Primary-with-nil-dependency is `fail` (fail-closed);
  a secondary one is `warn`.
- Account's "Valkey is required in prod" is `health.Cache(ping, true)`.
- CPU and memory warn above 90% or when unmeasurable ("could not tell" is not "fine"); they are never Primary.
- CPU is a 150ms two-sample delta (the since-boot average of the older copies hid saturation; account BUG-040);
  `-1` is never rounded (account BUG-041).
- Responses never contain a dependency's error text (unauthenticated routes); it goes to the log via
  `observability.Warn`. A panicking probe is contained and treated as an error.
- Not extracted: billing's `clock`/`today` extras. Add them as a custom `Check` locally.

## Migration

| Product | Replaced files |
|---|---|
| billing | `api/internal/api/v1/health.go` (handlers `health`/`healthCheck`, keep `clock` as a local `Check`) |
| wallet | `api/internal/api/v1/health.go` |
| poker | `api/internal/api/v1/health.go` |
| dfe | `api/internal/api/v1/health.go` |
| account | `api/cmd/api/health.go` |

Behavior differences to expect: `Content-Type: application/health+json` everywhere, and `measurementName`/units
spelled `millisecond` (account used `ms`).
