# resilient

The HTTP client every CTech service uses to call another service: a tuned `http.Transport`, a circuit breaker
per host (or per endpoint), retry of idempotent calls with backoff, jitter and `Retry-After`, header injection
and an optional short negative cache. Replaces the retry loops each product wrote locally.

## When to use

Any outbound HTTP call to another CTech service (account, billing, wallet, KYC, DFe). Not for streaming or
websocket upgrades. Do not write a retry loop in a product ("hand-rolled retry/backoff" is a review reject).

## API

Existing, unchanged: `Client(timeout, Config)`, `NewTransport()`, `Wrap(rt, Config)`, `Config`, `State`,
`ErrCircuitOpen`, `OpenError`.

Added (all additive):

| Symbol | What |
|---|---|
| `NewHTTPClient(opts...) *http.Client` | transport + breaker + retry + headers (+ negative cache) |
| `NewRetryTransport(opts...) http.RoundTripper` | same stack as a RoundTripper, for your own `http.Client` |
| `WithTimeout`, `WithMaxAttempts`, `WithBackoff`, `WithMaxRetryAfter` | retry and total timeout |
| `WithDialTimeouts(dial, tls, header)` | transport timeouts |
| `WithBreaker(Config)`, `WithoutBreaker`, `WithPerEndpointCircuit` | breaker |
| `WithIdempotencyKeyFunc(fn)`, `DeterministicKey(parts...)` | Idempotency-Key injection |
| `WithRequestID(fn)` | X-Request-Id source (default `observability.RequestID(ctx)`) |
| `WithNegativeCache(ttl, statuses...)` | short cache of e.g. 404 on GET/HEAD |
| `Config.Key`, `HostKey`, `EndpointKey` | which circuit a request belongs to |
| `Default*` constants, `HeaderIdempotencyKey`, `HeaderRequestID` | named defaults and headers |

Stack, outermost first: retry (+ headers) -> negative cache -> breaker -> transport. The breaker is under the
retry: every attempt counts, and an open circuit ends the retries at once with `ErrCircuitOpen`.

## Retry rules

- Retried: a **safe method** (GET, HEAD, OPTIONS, TRACE), or **any request with an `Idempotency-Key`**, on a
  transport error or a 5xx other than 501/505. Default 3 attempts, backoff `rand[0, min(2s, 100ms*2^n)]`.
- **Never a 4xx** (including 408 and 429). Poker's loop retried 408/425/429; this one does not, by rule. A 429's
  `Retry-After` is for the caller to honor.
- `Retry-After` (seconds or HTTP date) replaces the backoff. If it is longer than `WithMaxRetryAfter`
  (default 2s) the client does not wait: it returns that response, so the caller decides.
- A body without `GetBody` is sent once. `http.NewRequest` with `bytes.Reader`/`strings.Reader` sets it.
- Cancelled context, `context.Canceled` and `ErrCircuitOpen` are never retried.

## Fail-open / fail-closed

The client itself **fails closed**: when the circuit is open or the retries are spent you get the error (or the
last response), never a guess. The decision "no answer means allow" belongs next to each call and must be
written down in the product's dependency table. Family rule: reads of non-critical data may fail open;
money/PII mutations, auth and KYC decisions fail closed. A KYC or entitlement lookup failing must never be read
as "not found".

The negative cache is stale for at most `ttl` (default 5s). Do not cache a status that a write made by the same
process flips (e.g. 404 on a resource you just created); the key includes a hash of `Authorization`, so one
caller's answer is never served to another.

## Example

```go
hc := resilient.NewHTTPClient(
    resilient.WithTimeout(6*time.Second),
    resilient.WithDialTimeouts(2*time.Second, 2*time.Second, 3*time.Second),
    resilient.WithPerEndpointCircuit(),
    resilient.WithNegativeCache(5*time.Second, http.StatusNotFound),
)

req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1.0/internal/wallet/holds", bytes.NewReader(body))
// Deterministic: the same hand/user always replays instead of repeating.
req.Header.Set(resilient.HeaderIdempotencyKey, resilient.DeterministicKey("hold", userID, handID))
resp, err := hc.Do(req)
if errors.Is(err, resilient.ErrCircuitOpen) { /* decide fail-open or fail-closed here */ }
```

## Extracted from

- `ctech-poker/api/internal/walletclient` (`do`, `retryAfter`, `retryableStatus`): 3 attempts, jitter,
  Retry-After, breaker per endpoint, retry only when safe or keyed. Its breaker is replaced by the existing one
  with `WithPerEndpointCircuit`.
- The 1-retry-on-transport-error-or-5xx loops in `ctech-account/api/internal/billingclient`,
  `ctech-billing/api/internal/accountclient`, `ctech-wallet/api/internal/{billingclient,kycclient}` and the
  DFe clients: all become `NewHTTPClient` defaults.

## How each product migrates

Replace the local `http.Client`/`http.Transport` and the retry/breaker loop with `resilient.NewHTTPClient(...)`:

- poker: delete `do`, `retryAfter`, `retryableStatus`, `breakerAllows`, `recordResult`, `errCircuitOpen`,
  `breakers`, `retryDelay` from `walletclient/client.go`; map `errCircuitOpen` to `resilient.ErrCircuitOpen`.
  Keep the per-request `Idempotency-Key` it already sets.
- wallet: `billingclient/client.go`, `kycclient/kycclient.go` (kyc: fail closed, never "not found").
- billing: `accountclient/client.go`. account: `billingclient/client.go`. dfe: `billingclient`, `accountclient`.
- Callers that relied on a 4xx being retried (poker 408/425/429) must handle it themselves.

Bump `api-commons` to the release containing this package; the change is additive, so products on older
versions are unaffected until they bump.
