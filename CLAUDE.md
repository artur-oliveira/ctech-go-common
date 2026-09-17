# CLAUDE.md — ctech-go-common

This is a shared foundation library consumed by every CTech Go service — treat every change here (defaults,
behavior, error semantics, cache/lock backend choices, health checks) as a cross-repo contract change, not a local
tweak. Confirmed consumers today (`go.mod` pins as of this audit): `ctech-account/api` (`v1.9.1`), `ctech-dfe/api`
(`v1.9.1`), `ctech-wallet/api` (`v1.9.1`), `ctech-wallet/pix-gateway` (`v1.7.2`, lagging), `ctech-poker/api`
(`v1.9.2`), `ctech-billing/api` (`v1.8.0`). See `README.md`/`AGENTS.md` for the full package inventory and API surface.

## CTech Family — Cross-Repo Awareness (IMPORTANT)

This repo is one service in the CTech product family, not an isolated project. All CTech repos live under the same GitHub account and are meant to be treated as one codebase split across repos:

- ctech-cdk (github.com/artur-oliveira/ctech-cdk) — shared CDK constructs (EC2/ASG, DynamoDB, etc.)
- ctech-go-common (github.com/artur-oliveira/ctech-go-common) — shared Go libraries (HTTP client, auth, retries, websocket drain, caching)
- ctech-account, ctech-wallet, ctech-billing, ctech-dfe, ctech-poker — backend services
- ctech-ui (github.com/artur-oliveira/ctech-ui) — shared frontend design system / components (adoption in progress)
- ctech-ws-client (github.com/artur-oliveira/ctech-ws-client) — shared websocket client library
- ctech-oauth-client, ctech-vanity, ctech-lbalancer — supporting infra/clients

Before making a decision here, ask: "does this apply to the whole family, not just this repo?" Treat as cross-repo by default:
- Infra/runtime bugs (clock drift, spot interruption handling, websocket draining, health checks, load balancer behavior) — check ctech-cdk / ctech-lbalancer and sibling services for the same exposure before treating it as local.
- API leaks/perf/cost bugs (DynamoDB read/write amplification, KMS decrypt calls, SQS growth, N+1 requests) — check whether the root cause is shared code (ctech-go-common) or a repeatable pattern other services also have.
- Frontend state/websocket/resilience/UX patterns (reconnect, circuit breaker, error/loading/empty states, 404/500/503 pages, OAuth flow, modals, buttons) — check ctech-ui and ctech-ws-client for the shared version before implementing locally.
- New reusable code (not service-specific business logic) — default to proposing it for a shared package (ctech-cdk, ctech-go-common, ctech-ui, ctech-ws-client) instead of duplicating it here.

A fix scoped to only this repo, for a problem that is actually systemic across the family, is an incomplete fix. This applies to AI agents working in single-repo sessions too.

## Error-semantics rules (cross-repo contract)

- **A cancelled DynamoDB transaction is not a verdict — classify it by cancellation reason.**
  `dynamo.IsConditionFailed` covers only a genuine conditional-check failure (single-item
  `ConditionalCheckFailedException`, or a `TransactionCanceledException` whose reasons include
  `ConditionalCheckFailed`). `dynamo.IsTransactionConflict` (another transaction was operating on one of the
  items) and `dynamo.IsTransactionThrottled` (`ThrottlingError`/`ProvisionedThroughputExceeded`) mean the write
  never happened and no condition was evaluated: retry it, do not reconcile. Until 2026-09-17
  `IsConditionFailed` returned true for *any* `TransactionCanceledException`, and every consumer that treats
  "condition failed" as "someone else won the race" inherited the bug — `ctech-poker`'s
  `tablestore.resolveCommitErr` mapped a plain transaction conflict to its `ErrVersionConflict`, whose handlers
  reconcile and move on, so an all-in runout step that was never written looked like a street a sibling had
  already dealt and the hand froze mid-runout with chips committed (`ctech-poker`
  `docs/specs/2026-09-17-frozen-table-runout-and-sitout-fold.md`). When adding a new error predicate here, ask
  what the caller will *do* with a true answer: a retryable failure and a lost race must never share one.
