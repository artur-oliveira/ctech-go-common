# Erasure lock hook (v1.14.0) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a participant react the moment one of its users becomes locked. ctech-poker needs it to force-close WebSockets that were already open (user decision, 2026-10-08). The same release fixes the erasure docs that drifted from the ctech-account contract.

**Architecture:** `Consumer.OnLock(fn)` registers a callback. `handle` calls it after a `user.locked` message, or the synthetic lock applied before a `user.erase`, changes the sub's state **to** `locked`. A redelivery, a stale lock or an already-locked/erased sub does not call it. The callback is best-effort: it cannot fail the message, because the lock state is already persisted.

**Tech Stack:** Go 1.27, api-commons `erasure`.

**Spec:** `ctech-account/docs/specs/2026-10-06-account-deletion-saga-protocol.md` §4.2; consumer: `ctech-poker/docs/plans/2026-10-07-account-deletion-participant.md`.

## Global Constraints

- Branch `feat/erasure-lock-hook`. Conventional Commits, **no `Co-Authored-By` / Claude attribution**.
- Additive API only. Existing callers of `NewConsumer` behave exactly as before.
- `go vet ./... && go test ./... -race -count=1` green.
- Release `v1.14.0` (merge, push and tag) only after asking the user.

## Review Focus

1. **A redelivered `user.locked`** must not fire the hook a second time. Test: `TestConsumer_OnLockFiresOnTransitionOnly`.
2. **An erase arriving with no prior lock** fires the hook once, from the synthetic lock, before the purge. Test: same test.
3. **A panicking callback** must not crash the consumer loop or lose the message. Test: `TestConsumer_OnLockPanicIsContained`.

---

### Task 1: `Consumer.OnLock`

**Files:** Modify `erasure/consumer.go`; Test `erasure/consumer_test.go`.

**Interfaces:** Produces `func (c *Consumer) OnLock(fn func(ctx context.Context, sub string)) *Consumer`.

- [ ] **Step 1: Failing tests** — append to `erasure/consumer_test.go`:

```go
func TestConsumer_OnLockFiresOnTransitionOnly(t *testing.T) {
	ctx := context.Background()
	c, _, _ := newTestConsumer(t, "wallet", &purgeRecorder{ack: Ack{Result: ResultDone}})
	var fired []string
	c.OnLock(func(_ context.Context, sub string) { fired = append(fired, sub) })

	lock := body(t, msgAt(TypeLocked, "r1", 0))
	if err := c.handle(ctx, lock); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if err := c.handle(ctx, lock); err != nil { // redelivery
		t.Fatalf("lock again: %v", err)
	}
	if len(fired) != 1 || fired[0] != "user-1" {
		t.Fatalf("fired %v, want exactly once for user-1", fired)
	}

	c2, _, _ := newTestConsumer(t, "wallet", &purgeRecorder{ack: Ack{Result: ResultDone}})
	var fired2 int
	c2.OnLock(func(context.Context, string) { fired2++ })
	if err := c2.handle(ctx, body(t, msgAt(TypeErase, "r2", 0))); err != nil {
		t.Fatalf("erase: %v", err)
	}
	if fired2 != 1 {
		t.Fatalf("an erase with no prior lock must fire once (synthetic lock), fired %d", fired2)
	}
}

func TestConsumer_OnLockPanicIsContained(t *testing.T) {
	c, store, _ := newTestConsumer(t, "wallet", &purgeRecorder{})
	c.OnLock(func(context.Context, string) { panic("boom") })
	if err := c.handle(context.Background(), body(t, msgAt(TypeLocked, "r1", 0))); err != nil {
		t.Fatalf("a panicking hook must not fail the message: %v", err)
	}
	if blocked, _ := store.Blocked(context.Background(), "user-1"); !blocked {
		t.Fatal("the lock must be persisted regardless of the hook")
	}
}
```

- [ ] **Step 2: Fail** — `go test ./erasure/ -run OnLock -count=1` → build FAIL `c.OnLock undefined`.

- [ ] **Step 3: Implement** — in `erasure/consumer.go`:

1. Add the field `onLock func(ctx context.Context, sub string)` to `Consumer`, and:

```go
// OnLock registers fn, called when a message changes a user's state to
// locked: a user.locked, or the synthetic lock applied before a user.erase.
// Redeliveries and stale locks do not call it. Use it for immediate side
// effects such as closing open sockets; it runs after the lock is persisted,
// cannot fail the message, and a panic in it is recovered and logged.
func (c *Consumer) OnLock(fn func(ctx context.Context, sub string)) *Consumer {
	c.onLock = fn
	return c
}

// applyLock applies a lock-type message and fires the hook on a transition.
func (c *Consumer) applyLock(ctx context.Context, key string, m Message) (Record, error) {
	before, err := c.store.Get(ctx, key)
	if err != nil {
		return Record{}, err
	}
	rec, err := c.store.Apply(ctx, key, m)
	if err != nil {
		return Record{}, err
	}
	if c.onLock != nil && rec.State == StateLocked && (before == nil || before.State != StateLocked) {
		func() {
			defer func() {
				if p := recover(); p != nil {
					slog.ErrorContext(ctx, "erasure: OnLock hook panicked", "service", c.service, "panic", fmt.Sprint(p))
				}
			}()
			c.onLock(ctx, m.Sub)
		}()
	}
	return rec, nil
}
```

2. In `handle`:
- the non-erase branch becomes:

```go
	if m.Type != TypeErase {
		if m.Type == TypeLocked {
			_, err := c.applyLock(ctx, key, m)
			return err
		}
		_, err := c.store.Apply(ctx, key, m)
		return err
	}
```

- the synthetic lock line `rec, err := c.store.Apply(ctx, key, lock)` becomes `rec, err := c.applyLock(ctx, key, lock)`.

`before` comes from a separate read and `Apply` re-reads inside its retry loop. A concurrent consumer can make both observe the transition, firing the hook twice. The hook must therefore be idempotent (closing already-closed sockets is). State this in the doc comment above and add `// ponytail: Get+Apply is not atomic; hooks must be idempotent.`

- [ ] **Step 4: Pass** — `go vet ./... && go test ./... -race -count=1` → all `ok`.
- [ ] **Step 5: Commit** — `git add erasure && git commit -m "feat(erasure): OnLock hook fired when a user becomes locked"`

---

### Task 2: Docs fixes and the hook

**Files:** Modify `README.md` ("Account erasure"), `AGENTS.md` (erasure entry), `erasure/ack.go` (doc comment), `CLAUDE.md` (consumer versions, if it lists them).

- [ ] **Step 1:** `README.md`, "Account erasure":
  - In step 1, replace "(filter policy on `services`, scope `MessageBody`; raw delivery recommended, the envelope is also accepted)" with "(raw message delivery; filter policy `{"services": ["<service>"]}` on the `services` **message attribute**, which ctech-account sets on every publish; the SNS envelope is also accepted)".
  - Name the state table per service, e.g. `{env}_<service>_erasure_state`: participants share one AWS account, and `NewStore`'s `tablePrefix` must carry the service name.
  - Step 3: the ack scope is `internal:account:erasure-ack`, on a **dedicated** confidential client.
  - Step 4: ctech-account calls the eligibility endpoint with a token it mints: `aud` = the participant's audience, scope `internal:<service>:erasure-eligibility`, `iss` = ctech-account's `APP_URL`.
  - Add: "Call `consumer.OnLock(fn)` for immediate reactions to a lock (closing sockets); fn must be idempotent."
- [ ] **Step 2:** `erasure/ack.go` doc comment: `(scope account:erasure:ack)` → `(scope internal:account:erasure-ack, on a dedicated client)`.
- [ ] **Step 3:** `AGENTS.md`: add `OnLock` to the `Consumer` mention.
- [ ] **Step 4:** `go vet ./... && go test ./... -race -count=1` → green.
- [ ] **Step 5: Commit** — `git add README.md AGENTS.md erasure/ack.go CLAUDE.md && git commit -m "docs(erasure): attribute filter, scope names, OnLock"`
- [ ] **Step 6: Release — ask the user first:** merge `feat/erasure-lock-hook` into `main` (fast-forward), push, tag `v1.14.0`, push the tag.
