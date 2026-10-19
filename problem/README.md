# problem

RFC 7807 / 9457 Problem Details for every CTech Go service: one body shape, one Fiber v3 sender and error handler,
and a per-product type registry, so no product needs its own `internal/problem` or `internal/apierror` package.
Everything added here is additive; existing symbols (`New`, `BadRequest`, `Validation`, `TooManyRequestsAfter`,
`WithNextAction`, `WithCause`, the `Type*` and `NextAction*` constants) are unchanged.

## When to use

Any HTTP error response. Return a `*problem.Problem` (it is an `error`) from a handler and install
`problem.ErrorHandler`; do not write error JSON by hand.

## API

- `Problem` fields: `type, title, status, detail, instance, code, errors[], next_action, retry_after_seconds,
  required_scope, max_age_seconds, min_amount, max_amount`, plus arbitrary extras (`WithExtra`).
- Fluent setters: `WithCode`, `WithInstance`, `WithRequiredScope`, `WithMaxAge(time.Duration)`, `WithErrors`,
  `WithNextAction`, `WithExtra(key, value)`, `WithCause`. `Cause()` reads the cause back.
- `FieldError{field, code, message, params, tag}` and `Field(field, code, message, kv...)`.
- Constructors: `BadRequest`, `Unauthorized`, `Forbidden`, `NotFound`, `Conflict`, `UnprocessableEntity`,
  `UnprocessableEntityFields(detail, errs...)`, `Validation(errs)`, `TooManyRequests`, `TooManyRequestsAfter`,
  `ServiceUnavailable`, `GatewayTimeout`, `InternalServer`, `NotImplemented`, `PayloadTooLarge`, `PaymentRequired`,
  `PreconditionFailed`, `New`.
- Registry: `NewRegistry(baseURI)`, `Register(slug, status, title) uri`, `New(slug, detail)`, `URI`, `Lookup`,
  `CodeFromType`. A registered problem gets `code` = slug in snake_case.
- Fiber v3: `(*Problem).Send(c)`, `FromError(err)`, `ErrorHandler`, `MarshalBody()`, `ContentType`.

## Decisions (fail-open / fail-closed)

- **Unknown errors fail closed**: any error that is not a `*Problem` or `*fiber.Error` becomes a generic 500
  ("an unexpected error occurred"). Its text is logged (with request id, via `observability`) as the cause and
  never serialized. A `*fiber.Error` 5xx other than 503/504 is also generic; 4xx keep status and fiber's message.
- `context.DeadlineExceeded` (anywhere in the chain) is a retryable 504 (`next_action: retry`).
- `Send` sets `Retry-After` whenever `retry_after_seconds > 0`, so body and header cannot disagree.
- If a body cannot be marshalled (e.g. a non-encodable `WithExtra` value), `Send` still answers a static
  `application/problem+json` 500 rather than Fiber's plain-text default.
- A `Problem` with a status outside 400-599 is sent as 500.
- Extras never override a typed member on a key clash.
- `Problem` has no `MarshalJSON` on purpose: products embed it and add fields, and a promoted `MarshalJSON`
  would silently drop theirs. Consequence: `json.Marshal(p)` omits `WithExtra` members; `Send`/`MarshalBody` include them.
- `Registry.Register`/`New` panic on a conflicting or unknown slug: a programming error, found at init.

## Example

```go
var types = problem.NewRegistry("https://billing.aoctech.app/problems/")

var (
	_ = types.Register("no-billing-account", http.StatusForbidden, "No Billing Account")
	_ = types.Register("tenant-suspended", http.StatusForbidden, "Tenant Suspended")
)

app := fiber.New(fiber.Config{ErrorHandler: problem.ErrorHandler})

func handler(c fiber.Ctx) error {
	if bad {
		return problem.UnprocessableEntityFields("invalid request",
			problem.Field("items[0].quantity", "too_small", "must be at least 1", "min", 1))
	}
	if limited {
		return problem.TooManyRequestsAfter("slow down", 30) // Retry-After: 30
	}
	if suspended {
		return types.New("tenant-suspended", "live billing is suspended").
			WithExtra("blockers", blockers)
	}
	if err := svc.Do(ctx); err != nil {
		return err // unknown -> generic 500, cause logged; deadline -> 504
	}
	return nil
}
```

## Extracted from and migration

Union of ctech-billing `internal/problem`, ctech-wallet `internal/problem`, ctech-dfe `internal/problem`,
ctech-poker `internal/problem` and ctech-account `internal/apierror`.

| Product | Replaces | Keeps locally |
| --- | --- | --- |
| billing | `Problem` wrapper, `Send`, `Field`/`FieldError`, `WithCode`/`slugCode`, `Unprocessable`/`Internal`, ErrorHandler glue | `FromError` domain mapping (build on `problem.New`/registry), `Blockers`/`UpgradePlan`/`Quota` via `WithExtra` |
| wallet | `Problem` wrapper, `Send`, `FromFiber`, `GatewayTimeout`, `ServiceUnavailable`, `StepUpRequired` (`WithMaxAge`), min/max amounts (already on `Problem`) | wallet type constants and constructors, as registry entries |
| dfe | `Problem` wrapper, `Send`, `FromFiber`, `PayloadTooLarge`, `PaymentRequired`, `NotImplemented`, `GatewayTimeout` | `Reason`/`Meter`/`Plan`/quota members via `WithCode`/`WithExtra` |
| poker | `Problem` wrapper, `Send`, `FromError`/`InternalServer(detail, c, err)` deadline mapping, `GatewayTimeout` | `FromWalletError`, `FromResponsibleError`, `TableFull` |
| account | `apierror.Problem`, `Send`, `NewFromFiber`, `WithCause`, `TooManyRequests`, `StepUpRequired`, `InsufficientScope`, `PlanLimit` code | OAuth `error`/`error_description` members (via `WithExtra`), KYC/auth types as a registry on `https://accounts.aoctech.app/problems/` |

Known differences a migrating product must decide on: wallet/dfe override `Error()` to return detail only (shared
`Error()` is `Title: Detail`); account's `Problem.Error()` is detail only too; billing's `code` is always present
(use the registry, or `WithCode(problem.CodeFromType(...))`); account's `max_age_seconds`/`retry_after_seconds`
were `int64` and are `int` here; account and billing use absolute type URIs, the others relative `/problems/...`
(clients that branch on `type` must keep their current values: pick the registry base accordingly).
