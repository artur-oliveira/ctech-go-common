# validation

Shared request validation for CTech Go services: one `go-playground/validator/v10` singleton, **stable error
codes** (not free text), a 422 `problem` conversion, a strict JSON binder, a free-text sanitizer and the common
Brazilian validators.

Use it in every HTTP handler that accepts a JSON body. Do not keep a per-service `validate`/`validation` package.

## API

| Symbol | Purpose |
|---|---|
| `BindJSON[T](body, *T) *problem.Problem` | strict decode (`DisallowUnknownFields`, no trailing data, 1 MiB cap) then `Struct` |
| `BindJSONLimit[T](body, *T, maxBytes)` | same, explicit cap |
| `Struct(v) *problem.Problem` | nil if valid; 422 with `errors[]` (`Tag` = stable code); 500 on programmer error |
| `Validate(v) error` | same without HTTP: `*ValidationError{Fields []FieldError}` |
| `IsValidationError(err)` | `errors.AsType` helper |
| `ToProblem(err)` | `Validate` error to problem |
| `Register(tag, fn, code)` | add a service-specific rule (e.g. fiscal CFOP/NCM) and its code. Call at init |
| `FieldError{Field, Code, Tag, Param}` | local type (decoupled from `problem.FieldError`); `.Message()` is the default English text of the code |
| `Freetext(s, FreetextRule{Min, Max, AllowNewlines})` | trim + length (runes) + no CRLF injection + junk filter; errors wrap `ErrFreetext*` |
| `IsValidCPF/CNPJ/Phone/Zip(string)` | plain functions behind the tags |
| `Code*` / `Tag*` constants | the stable vocabulary |

Tags: `cpf`, `cnpj` (alphanumeric, IN RFB 2229/2024), `cpfcnpj`, `phone` (BR, DDD + 8/9 digits), `zip` (CEP, 8
digits), `money_cents` / `money_cents=<max>` (integer kinds only, `> 0`, default cap `MaxMoneyCents` = 2^53-1),
plus the stock `uuid` and `ulid`. Field paths use the `json` tag name (`items[0].amount`).

Stable codes: `required`, `too_short`, `too_long`, `out_of_range`, `invalid_value`, `invalid_list`,
`invalid_format`, `invalid_email`, `invalid_url`, `invalid_cpf`, `invalid_cnpj`, `invalid_tax_id`,
`invalid_phone`, `invalid_zip`, `invalid_ulid`, `invalid_uuid`, `invalid_money`, and `invalid` (any unmapped
tag; `FieldError.Tag` keeps the raw rule).

## Fail-open / fail-closed

- Body over the cap, unknown field, trailing data, malformed JSON: **closed** (400/413). The client detail is
  generic and never echoes the parse error or the body (PII, struct names); the cause is logged with `slog`
  (never the body) and kept in `Problem.Cause()`.
- A non-struct passed to `Validate`: **closed** as a 500 problem, never as "valid".
- A malformed `money_cents=<param>`: rejects every value (closed).
- Documents tolerate formatting (`.`, `-`, `/`, space) but reject any other character. Repeated-digit CPF/CNPJ
  are rejected.

## Example

```go
type CreateCharge struct {
    Amount int64  `json:"amount" validate:"money_cents=100000000"`
    Payer  string `json:"payer_tax_id" validate:"required,cpfcnpj"`
    Note   string `json:"note" validate:"max=200"`
}

func handler(c fiber.Ctx) error {
    var in CreateCharge
    if p := validation.BindJSON(c.Body(), &in); p != nil {
        return writeProblem(c, p) // 400 / 413 / 422 with errors[]
    }
    note, err := validation.Freetext(in.Note, validation.FreetextRule{Min: 3, Max: 200})
    ...
}

func init() { _ = validation.Register("cfop", cfopFunc, "invalid_cfop") } // fiscal rules stay in the service
```

## Extracted from / migration

| Product | Replaced by this package |
|---|---|
| ctech-wallet `api/internal/validation/*` and `bindJSON` in `api/internal/api/v1/helpers.go` | `validation.BindJSON(c.Body(), &dst)`; keep UF/timezone sets and fiscal tags via `Register`. Messages were Portuguese free text; now codes (UI maps code to text) |
| ctech-dfe `api/internal/validation` (generic part: `ValidCPF`, `ValidCNPJ`, cep/phonebr/money) | `IsValidCPF/CNPJ`, tags `cpf`/`cnpj`/`zip`/`phone`; fiscal regex tags (cfop, ncm, cest, ...) stay in dfe and register through `Register`. NOTE: dfe maps CNPJ letters to 10-35; the RFB rule is ASCII-48 (A=17), implemented here, so dfe's alphanumeric check digits differ from this package's |
| ctech-account `api/internal/validate` (`Freetext`, `IsValidationError`, `Struct`) | `validation.Freetext`, `IsValidationError`, `Validate` (error carries `Fields[]` instead of a joined sentence) |
| ctech-account `api/internal/domain/kyc/cpf.go` (`IsValidCPF`) | `validation.IsValidCPF` (now also accepts formatted input) |
| ctech-billing `api/internal/api/v1/validate.go` | only the generic checks map here (required/too_long/invalid_email/invalid_id style codes, positive-cents amount). Its imperative `checks` collector stays; adopt the code names. Billing-specific rules (webhook URL, dates, tax-ID shape-only mode) stay in billing |

Bumping consumers: add `gopkg.aoctech.app/api-commons` at the release containing this package; the module gains
the `github.com/go-playground/validator/v10` dependency (already used by wallet, dfe and account).
