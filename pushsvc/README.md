# pushsvc

The product-independent half of CTech Web Push. It sits on [`webpush`](../webpush/README.md) (stateless crypto and VAPID) and
`dynamo.Base`, and holds no business rule: each product decides who gets what, and when.

Design: `ctech-dfe/docs/specs/2026-11-17-web-push.md` (event-driven pushes) and
`ctech-poker/docs/specs/2026-11-18-scheduled-web-push.md` (scheduled reminders).

| Piece | What it is |
|---|---|
| `Store` | Subscriptions: `USER#{sub}` / `SUB#{id}`, a count row, 10 devices a person, 60-day TTL renewed by the sync |
| `Service`, `Mount` | `GET /push/config`, `PUT\|GET /push/subscriptions`, `DELETE /push/subscriptions/:id` |
| `Job`, `BuildPayload` | Notification contract v1 (`webpush/testdata/notification-v1.json`), 1 KB budget |
| `Handler`, `LoadSender`, `Lambda` | The push-sender Lambda core: devices of each recipient, send, delete what is `Gone` |
| `Invoker` | Async invoke of the sender (`Event`, expects 202) |
| `ReminderStore`, `SlotOf`, `ParseHHMM`, `SlotsToProcess`, `Today`, `Tick` | Scheduled reminders in 15-minute São Paulo slots |
| `cmd/push-keygen` | Prints the `aws ssm put-parameter` commands for a service's VAPID pair; writes nothing |

## The table

One table per product, `{prefix}_push`, `pk`/`sk` strings, TTL attribute `ttl`, plus two GSIs the product must define:

| Index | Keys | Projection | Use |
|---|---|---|---|
| `endpoint-index` | `endpoint_id` / `pk` | `KEYS_ONLY` | who else holds this browser endpoint |
| `slot-index` | `slot_key` / `kind_sub` | `KEYS_ONLY` | reminders due in a slot; sparse, only enabled ones |

The endpoint and keys are not sealed (a push service refuses a send without the VAPID private key, and the table is
encrypted at rest). Logs carry `sub_hash` and the push host only, never an endpoint, key or payload.

## Subscribing

```go
store := pushsvc.NewStore(db, prefix)
svc := pushsvc.NewService(store, publicKeyFromSSM) // "" leaves push off: /push/config says enabled=false, Subscribe answers 503
pushsvc.Mount(router, svc, authMw, func(c fiber.Ctx) string { return userID(c) }, scopeCheck)
```

An endpoint moves from one person to another (a shared browser) only when the request carries the keys it was registered
with; otherwise `Subscribe` answers 409 and the owner keeps it.

## A scheduled push (what a product's tick does)

The product writes its rule (`Decide`) and the Lambda main; `Tick` does the rest.

```go
rs := pushsvc.NewReminderStore(db, prefix)
tick := pushsvc.NewTick(rs, pushsvc.NewStore(db, prefix), rules, invoker). // rules.Decide(ctx, now, reminder) (*Job, error)
    OnSent(func(ctx context.Context, r pushsvc.Reminder) { /* optional: Poker moves a learned hour here */ })
err := tick.Run(ctx, time.Now())                                            // from the Scheduler-invoked Lambda
```

`Run` reads this slot and the one before (never across midnight), skips a person with no device (the day stays open), asks
`Decide` (nil means "not today", also leaving the day open), marks the day with `MarkSent` **before** handing the job to the
`Invoker` (a lost invoke is a push not sent, never one sent twice), and keeps going when one person fails. Only an unreadable
slot returns an error.

`Put` keeps `last_sent_day`, so moving a reminder's time cannot send twice in a day. Time is `America/Sao_Paulo` for the whole
family (a per-user zone is a later change).

## The sender Lambda

```go
h := pushsvc.NewHandler(pushsvc.NewStore(db, prefix), nil, alertsPublisher)
lambda.Start(pushsvc.Lambda(h, ssm.NewFromConfig(cfg), pushsvc.Paths{
    Private: os.Getenv("PUSH_VAPID_PRIVATE_KEY_SSM_PATH"), Previous: os.Getenv("PUSH_VAPID_PRIVATE_KEY_PREVIOUS_SSM_PATH"),
    Public: os.Getenv("PUSH_VAPID_PUBLIC_KEY_SSM_PATH"), Subject: os.Getenv("PUSH_SUBJECT_SSM_PATH"),
}))
```

The keys are read on the first invocation, not at start: a function deployed before the operator wrote the parameters stays
quiet. A transient SSM failure is retried on the next invocation. Only a failure of the handler's own work (the table) returns
an error, so Lambda retries; a push service that refuses a device does not.

## Tests

Unit tests run anywhere. The integration tests need DynamoDB Local:

```
docker compose -f docker-compose.test.yml up -d
DYNAMODB_ENDPOINT=http://localhost:8131 go test -tags integration -race -count=1 ./pushsvc/
```
