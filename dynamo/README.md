# dynamo: conditional update and cursor pagination

Additive helpers on top of the existing `dynamo.Base`. Nothing existing changed (transaction error
classification included).

## ConditionalUpdate / UpdateVersioned (`conditional.go`)

| Symbol | Use |
|---|---|
| `(*Base).ConditionalUpdate(ctx, pk, sk, updates, condition, names, values) (bool, error)` | One `UpdateItem` guarded by your own `ConditionExpression` (single-use tokens, uniqueness, "status must be X"). |
| `(*Base).UpdateVersioned(ctx, pk, sk, mutate, maxAttempts) error` | Read, `mutate(item)` returns the changes, write guarded by `version`. |
| `VersionAttr`, `DefaultMaxAttempts`, `ErrItemNotFound`, `ErrVersionConflict` | Constants and sentinels. |

Retry policy (the decision that matters):

- `ConditionalUpdate`: condition not met returns `(false, nil)` and is **never retried**: it is the caller's
  verdict. Only `TransactionConflict` (nothing written, no condition evaluated) is retried, at most
  `DefaultMaxAttempts` times with linear backoff that honors `ctx`. Fail-closed: any other error is returned.
- `UpdateVersioned`: a lost version race re-reads and re-runs `mutate` (bounded), then `ErrVersionConflict`
  (map to 409). Business preconditions go inside `mutate`; an error from it is returned as is, never retried.
  `mutate` may run more than once, keep it side-effect free. Items without `version` count as 0 (the first write
  creates it). A missing item is `ErrItemNotFound`, never created.

```go
sk := "ACTIVE"
ok, err := repo.ConditionalUpdate(ctx, pk, &sk,
    map[string]any{"used_at": now},
    "attribute_not_exists(#used_at)", map[string]string{"#used_at": "used_at"}, nil)
// ok == false: already used.

err = repo.UpdateVersioned(ctx, pk, &sk, func(it map[string]types.AttributeValue) (map[string]any, error) {
    // validate against the fresh item, return the new attributes
    return map[string]any{"balance": newBalance(it)}, nil
}, 0)
```

## Cursor and Page (`cursor.go`)

| Symbol | Use |
|---|---|
| `ParseLimit(raw)`, `ClampLimit(n)`, `DefaultPageLimit`=25, `MaxPageLimit`=100 | Page-size policy. |
| `EncodeCursor(lastEvaluatedKey, owner)`, `DecodeCursor(cursor, owner)` | Opaque URL-safe cursor bound to an owner tag. |
| `Page[T]`, `NewPage(items, lastKey, owner)` | `{"data": [], "next_cursor": null}` |
| `PageWithPrevious[T]`, `NewPageWithPrevious(items, lastKey, incoming, owner)` | Adds `previous_cursor`, `has_next`, `has_previous` (dfe contract). |

Decisions:

- **Fail-closed on a bad cursor.** `DecodeCursor` returns a 400 `*problem.Problem` (`Cause()` is
  `ErrInvalidCursor`) for a malformed cursor or one issued for another owner. The old product helpers silently
  restarted from page 1. An empty cursor is the start.
- The owner tag is `sha256(owner)[:8]` (hides the raw id). It is defense in depth, **not a signature**; DynamoDB
  itself rejects a start key outside the queried partition. Pass the same `owner` (org or user id) to decode and
  encode.
- Key types S, N and B survive the round trip (a numeric key stays exact). Any other type gives no cursor.
- `next_cursor: null` is the only end-of-list signal; a short page is not the end (filtered queries).
- `previous_cursor` carries only the previous page's key (the cursor does not grow with depth). It is null on
  pages 1 and 2: page 1 is reached with no cursor, which `has_previous` tells the client.
- Cursors use unpadded base64url. Product cursors (StdEncoding) become invalid at migration: clients get a 400
  and restart the listing.
- **No `Scan` in request paths.** Every list is a `Query`/`QueryGSI` over a key designed for it; `ScanRaw`
  is for back-office jobs only.

Usage in a Fiber handler:

```go
owner := orgID
start, err := dynamo.DecodeCursor(c.Query("cursor"), owner)
if err != nil { return problemOf(err).Send(c) }
res, err := repo.Query(ctx, dynamo.QueryOpts{PK: pk, Limit: dynamo.ParseLimit(c.Query("limit")), ExclusiveStartKey: start})
// decode res.Items into rows, then:
return c.JSON(dynamo.NewPage(rows, res.LastEvaluatedKey, owner))
```

## Extracted from, and migration

- `ctech-account/api/internal/database/conditional.go`: delete the file, use `Base.ConditionalUpdate` (same
  semantics; token clashes are now an error; transient conflicts are retried).
- `ctech-billing/api/internal/repositories/base.go` (`EncodeCursor`/`DecodeCursor`, `Page`): use the dynamo ones
  with the org id as owner; envelope `{data,next_cursor}` is `dynamo.Page`.
- `ctech-wallet/api/internal/api/v1/helpers.go` (next cursor only): replace with `NewPage`, `DecodeCursor`.
- `ctech-poker/api/internal/api/v1/helpers.go` (`cursorPayload`, `buildNextCursor`, `decodeCursor`, `limitParam`,
  `PaginatedResponse`): replace with `NewPageWithPrevious`, `DecodeCursor`, `ParseLimit`. Poker's default limit
  50 becomes 25.
- `ctech-dfe/api/internal/api/v1/helpers.go` (`buildNextCursor`, `prevCursorOf`, `decodeCursor`): replace with
  `NewPageWithPrevious`. dfe's envelope key is `items`; the shared one is `data`, so the frontend changes in step.

## Tests

`go test ./dynamo/ -race` (unit, fake DynamoDB over HTTP). Integration (DynamoDB Local):
`DYNAMODB_ENDPOINT=http://localhost:8131 go test -tags integration -race -count=1 ./dynamo/`.
