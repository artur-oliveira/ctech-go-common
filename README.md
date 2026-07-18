# ctech-go-common

Shared Go module for the CTech platform. Reconciles internal packages that were independently
duplicated (and drifted) between `ctech-dfe/api` and `ctech-wallet/api`:

| Package     | What it holds                                                                                   |
|-------------|-------------------------------------------------------------------------------------------------|
| `cache`     | `Backend` interface + in-memory and Valkey implementations                                      |
| `dynamo`    | DynamoDB persistence primitives (`Base`, `Query`, transact-item builders, `MarshalMapOmitNull`) |
| `problem`   | RFC 7807 Problem Details — generic constructors (`BadRequest`, `NotFound`, `Validation`, ...)   |
| `awsconfig` | AWS SDK v2 config load + DynamoDB client bootstrap (with local-endpoint override)               |
| `ws`        | WebSocket connection registry, fanned out across instances via Valkey Pub/Sub                   |
| `oauth2client` | Cached OAuth2 client_credentials token fetcher, shared across M2M callers                    |

## Import path

```
import "gopkg.aoctech.app/api-commons/dynamo"
```

Import as `gopkg.aoctech.app/api-commons`, **not** `github.com/artur-oliveira/ctech-go-common` —
the vanity path is served by [`ctech-vanity`](https://github.com/artur-oliveira/ctech-vanity)'s
`go-import` redirect and is what lets the backing repo move without breaking every consumer's
import path. `ctech-dfe` and `ctech-wallet` will switch their own module paths to
`gopkg.aoctech.app/*` too, in their own follow-up migration plans.

## What's intentionally NOT here

- `CRUDRepository[T]` (org-scoped generic CRUD wrapper) — `ctech-dfe`-specific (multi-tenant);
  `ctech-wallet` has no equivalent. Stays in `ctech-dfe`, built on top of `dynamo.Base`.
- Per-service `Clients` struct (which AWS services to wire up) — `ctech-dfe` and `ctech-wallet`
  use genuinely different service sets (S3/SQS/SNS/Lambda/SecretsManager vs. SSM-only). Only the
  config-load + DynamoDB-client bootstrap (`awsconfig`) is shared.
- Fiscal-specific (`NoCertificate`, `SefazRejection`) and wallet-specific
  (`InsufficientBalance`, `WalletBusy`, ...) `problem` constructors — these live in each
  consumer's own `problem` package, built on the generic constructors here.
- Auth/JWT middleware — `ctech-dfe`, `ctech-wallet`, and `ctech-account` have genuinely different
  trust models (multi-tenant RBAC vs. user+M2M vs. account's own OIDC core); only the underlying
  token validation primitives would ever be shared, and that extraction is out of scope here.

## Development

```bash
go build ./...
go vet ./...
go test ./...
```

## Deploy

There is no build/publish step — Go modules are source-distributed. A release is just a semver
git tag; `go get`/the module proxy fetches source directly from the tagged commit via the VCS,
and consumers compile it themselves.

On every release:

1. Land all changes on `main` — CI (`.github/workflows/ci.yml`) must be green.
2. Decide the version bump per [semver](https://semver.org/): `MAJOR` for breaking API changes
   (removed/renamed exported symbol, changed function signature), `MINOR` for backward-compatible
   additions, `PATCH` for fixes that don't change any exported API.
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
6. Bump the dependency in consumers (`ctech-dfe`, `ctech-wallet`) on their own schedule —
   this module has no auto-bump/auto-deploy hook into either repo.

## License

[Elastic License 2.0 (ELv2)](LICENSE.md) — same license as the other CTech repositories.
