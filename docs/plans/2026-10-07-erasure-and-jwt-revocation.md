# Erasure contract + JWT revocation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the shared building blocks of LGPD account deletion in `ctech-go-common`: a
`jwtverify` revocation check (immediate token cut-off) and a new `erasure` package (message
contract, per-service lock/tombstone state, SQS consumer, ack client) that wallet, dfe, billing
and poker will import.

**Architecture:** `jwtverify.Verifier` gains an optional revocation backend (Valkey DB 0) checked
after signature validation. It fails open in `VerifyClaims` and fails closed in the new
`VerifyClaimsStrict`. `erasure` holds the JSON contract, an `erasure_state` DynamoDB store whose
transitions are a pure function (ordered by `issued_at`, `erased` is terminal), a sequential
SQS long-poll consumer that deletes a message only after it was fully handled, and an ack
client using `oauth2client`. No orchestration logic: that stays in ctech-account.

**Tech Stack:** Go 1.27, aws-sdk-go-v2 (dynamodb, **sqs**: new dependency), golang-jwt v5,
existing `cache`, `dynamo`, `oauth2client` packages.

**Spec:** `ctech-account/docs/specs/2026-10-06-account-deletion-saga-protocol.md` (contract,
§3–§5, §9). Context: `ctech-account/docs/specs/2026-10-06-account-deletion-overview.md`.

## Global Constraints

- Module path `gopkg.aoctech.app/api-commons`. Never import `github.com/artur-oliveira/ctech-go-common`.
- Only new dependency allowed: `github.com/aws/aws-sdk-go-v2/service/sqs`.
- Messages carry **no personal data**: only `sub`, ids, scope, timestamps. Logs never print a message body; log `request_id` / `message_id`.
- Message schema version: `1`. Types: `user.locked`, `user.unlocked`, `user.erase`. Scopes: `account`, `service`.
- Revocation key: `ctech:jwt:revoked_sub:{sub}`, value = cut-off unix seconds, TTL `RevocationTTL = 20 * time.Minute`. Lives in Valkey **DB 0** (base URL, no DB suffix).
- A token is revoked when an entry exists for its `sub` and `iat <= cutoff` (missing `iat` counts as 0).
- Erasure state table: `{prefix}_erasure_state`, partition key `pk` (S) only, TTL attribute `ttl`.
- Classify DynamoDB failures with `dynamo.IsConditionFailed` only (repo CLAUDE.md error-semantics rule).
- CI runs `go vet ./...`, `go build ./...`, `go test ./... -race -count=1`. All must pass.
- Mandatory documentation policy: README + AGENTS.md updated in the same change.
- Commits: no `Co-Authored-By` or any Claude attribution trailer.

## Review Focus

1. **`user.unlocked` delivered before its `user.locked`** (standard SQS reorders). A cancelled user must end up active, not locked forever. Covered by Task 3's `TestApply_UnlockBeforeLock_StaleLockIgnored`.
2. **Ack POST fails after a successful purge.** The message must be redelivered, the purge re-run safely, and the ack resent. Covered by Task 5's `TestConsumer_AckFailureRedelivers`.
3. **Valkey unreachable.** General routes keep working (fail open); money routes using `VerifyClaimsStrict` refuse. Covered by Task 1's `TestRevocation_BackendDown`.
4. **SNS subscription without raw message delivery.** The body arrives wrapped in an SNS envelope and must still decode. Without this, every message would silently go to the DLQ. Covered by Task 2's `TestDecode_SNSEnvelope`.
5. **`user.erase` arrives but the `user.locked` never did, and the purge reports a blocker.** The sub must end **locked**, not erased and not active. Covered by Task 5's `TestConsumer_EraseBlocked_LocksWithoutErasing`.

---

### Task 1: `jwtverify` revocation check

**Files:**
- Modify: `jwtverify/verifier.go` (struct `Claims`, struct `Verifier`, `VerifyClaims`)
- Create: `jwtverify/revocation.go`
- Test: `jwtverify/revocation_test.go`

**Interfaces:**
- Consumes: `cache.Backend` (`cache/cache.go`).
- Produces:
  - `Claims.IssuedAt int64`
  - `const RevocationTTL = 20 * time.Minute`
  - `var ErrTokenRevoked, ErrRevocationUnavailable error`
  - `func Revoke(ctx context.Context, c cache.Backend, sub string, cutoff time.Time, ttl time.Duration) error`
  - `func Unrevoke(ctx context.Context, c cache.Backend, sub string) error`
  - `func (v *Verifier) WithRevocation(c cache.Backend) *Verifier`
  - `func (v *Verifier) VerifyClaimsStrict(ctx context.Context, tokenStr string) (*Claims, error)`

- [ ] **Step 1: Write the failing tests**

Create `jwtverify/revocation_test.go` (same `jwtverify_test` package as `verifier_test.go`, so it reuses `newJWKSServer`, `signToken`, `baseClaims`, `newVerifier`, `testIssuer`, `testAudience`):

```go
package jwtverify_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"testing"
	"time"

	"gopkg.aoctech.app/api-commons/cache"
	"gopkg.aoctech.app/api-commons/jwtverify"
)

type downBackend struct{}

var errDown = errors.New("valkey down")

func (downBackend) Get(context.Context, string) ([]byte, bool, error)  { return nil, false, errDown }
func (downBackend) Set(context.Context, string, []byte, int) error     { return errDown }
func (downBackend) Delete(context.Context, string) error               { return errDown }
func (downBackend) DeletePrefix(context.Context, string) error         { return errDown }
func (downBackend) Ping(context.Context) error                         { return errDown }

func revocationFixture(t *testing.T) (*rsa.PrivateKey, *jwtverify.Verifier, cache.Backend) {
	t.Helper()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	js := newJWKSServer(t)
	js.publish(&key.PublicKey, "kid-1")
	rev := cache.NewMemoryBackend(16)
	return key, newVerifier(js).WithRevocation(rev), rev
}

func tokenIssuedAt(t *testing.T, key *rsa.PrivateKey, sub string, iat time.Time) string {
	t.Helper()
	claims := baseClaims(sub, testIssuer, testAudience)
	claims["iat"] = iat.Unix()
	return signToken(t, key, "kid-1", claims)
}

func TestVerifyClaims_ExtractsIssuedAt(t *testing.T) {
	key, v, _ := revocationFixture(t)
	iat := time.Now().Add(-time.Minute)
	cl, err := v.VerifyClaims(context.Background(), tokenIssuedAt(t, key, "user-1", iat))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cl.IssuedAt != iat.Unix() {
		t.Errorf("IssuedAt = %d, want %d", cl.IssuedAt, iat.Unix())
	}
}

func TestRevocation_TokenIssuedBeforeCutoffRejected(t *testing.T) {
	ctx := context.Background()
	key, v, rev := revocationFixture(t)
	tok := tokenIssuedAt(t, key, "user-1", time.Now().Add(-time.Minute))
	if err := jwtverify.Revoke(ctx, rev, "user-1", time.Now(), jwtverify.RevocationTTL); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := v.VerifyClaims(ctx, tok); !errors.Is(err, jwtverify.ErrTokenRevoked) {
		t.Fatalf("VerifyClaims err = %v, want ErrTokenRevoked", err)
	}
	if _, err := v.VerifyClaimsStrict(ctx, tok); !errors.Is(err, jwtverify.ErrTokenRevoked) {
		t.Fatalf("VerifyClaimsStrict err = %v, want ErrTokenRevoked", err)
	}
}

func TestRevocation_TokenIssuedAtCutoffSecondRejected(t *testing.T) {
	ctx := context.Background()
	key, v, rev := revocationFixture(t)
	now := time.Now()
	_ = jwtverify.Revoke(ctx, rev, "user-1", now, jwtverify.RevocationTTL)
	if _, err := v.VerifyClaims(ctx, tokenIssuedAt(t, key, "user-1", now)); !errors.Is(err, jwtverify.ErrTokenRevoked) {
		t.Fatalf("err = %v, want ErrTokenRevoked for iat == cutoff", err)
	}
}

func TestRevocation_TokenIssuedAfterCutoffAccepted(t *testing.T) {
	ctx := context.Background()
	key, v, rev := revocationFixture(t)
	_ = jwtverify.Revoke(ctx, rev, "user-1", time.Now().Add(-time.Hour), jwtverify.RevocationTTL)
	if _, err := v.VerifyClaims(ctx, tokenIssuedAt(t, key, "user-1", time.Now())); err != nil {
		t.Fatalf("token issued after cutoff rejected: %v", err)
	}
}

func TestRevocation_OtherSubUnaffected(t *testing.T) {
	ctx := context.Background()
	key, v, rev := revocationFixture(t)
	_ = jwtverify.Revoke(ctx, rev, "user-1", time.Now(), jwtverify.RevocationTTL)
	if _, err := v.VerifyClaims(ctx, tokenIssuedAt(t, key, "user-2", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("other sub rejected: %v", err)
	}
}

func TestRevocation_UnrevokeRestoresAccess(t *testing.T) {
	ctx := context.Background()
	key, v, rev := revocationFixture(t)
	tok := tokenIssuedAt(t, key, "user-1", time.Now().Add(-time.Minute))
	_ = jwtverify.Revoke(ctx, rev, "user-1", time.Now(), jwtverify.RevocationTTL)
	if err := jwtverify.Unrevoke(ctx, rev, "user-1"); err != nil {
		t.Fatalf("Unrevoke: %v", err)
	}
	if _, err := v.VerifyClaims(ctx, tok); err != nil {
		t.Fatalf("token rejected after Unrevoke: %v", err)
	}
}

func TestRevocation_TokenWithoutIatRejectedWhenRevoked(t *testing.T) {
	ctx := context.Background()
	key, v, rev := revocationFixture(t)
	claims := baseClaims("user-1", testIssuer, testAudience)
	delete(claims, "iat")
	_ = jwtverify.Revoke(ctx, rev, "user-1", time.Now(), jwtverify.RevocationTTL)
	if _, err := v.VerifyClaims(ctx, signToken(t, key, "kid-1", claims)); !errors.Is(err, jwtverify.ErrTokenRevoked) {
		t.Fatalf("err = %v, want ErrTokenRevoked", err)
	}
}

func TestRevocation_BackendDown(t *testing.T) {
	ctx := context.Background()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	js := newJWKSServer(t)
	js.publish(&key.PublicKey, "kid-1")
	v := newVerifier(js).WithRevocation(downBackend{})
	tok := tokenIssuedAt(t, key, "user-1", time.Now())

	if _, err := v.VerifyClaims(ctx, tok); err != nil {
		t.Fatalf("VerifyClaims must fail open, got %v", err)
	}
	if _, err := v.VerifyClaimsStrict(ctx, tok); !errors.Is(err, jwtverify.ErrRevocationUnavailable) {
		t.Fatalf("VerifyClaimsStrict err = %v, want ErrRevocationUnavailable", err)
	}
}

func TestRevocation_NotConfiguredSkipsCheck(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	js := newJWKSServer(t)
	js.publish(&key.PublicKey, "kid-1")
	if _, err := newVerifier(js).VerifyClaimsStrict(context.Background(), tokenIssuedAt(t, key, "user-1", time.Now())); err != nil {
		t.Fatalf("strict verify without revocation backend: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./jwtverify/ -run 'Revocation|IssuedAt' -count=1`
Expected: build FAIL: `undefined: jwtverify.Revoke`, `v.WithRevocation undefined`, `cl.IssuedAt undefined`.

- [ ] **Step 3: Implement**

In `jwtverify/verifier.go`:

1. Add to `Claims` after `LastMFAAt`:

```go
	IssuedAt  int64  // iat, unix seconds; 0 if absent
```

2. Add to `Verifier` after `cache cache.Backend`:

```go
	revocation cache.Backend // nil disables the revocation check; see WithRevocation
```

3. Rename the body of `VerifyClaims` into `verify(ctx, tokenStr string, strict bool)` and add the two entry points. Replace the current `func (v *Verifier) VerifyClaims(...)` header with:

```go
// VerifyClaims validates a raw JWT string and returns its parsed claims. When a
// revocation backend is configured and unreachable, the check is skipped (fail
// open): authentication must not depend on cache availability.
func (v *Verifier) VerifyClaims(ctx context.Context, tokenStr string) (*Claims, error) {
	return v.verify(ctx, tokenStr, false)
}

// VerifyClaimsStrict is VerifyClaims for money-moving routes: an unreachable
// revocation backend rejects the token with ErrRevocationUnavailable.
func (v *Verifier) VerifyClaimsStrict(ctx context.Context, tokenStr string) (*Claims, error) {
	return v.verify(ctx, tokenStr, true)
}

func (v *Verifier) verify(ctx context.Context, tokenStr string, strict bool) (*Claims, error) {
```

4. At the end of `verify`, replace `return cl, nil` with:

```go
	if iat, ok := mc["iat"].(float64); ok {
		cl.IssuedAt = int64(iat)
	}
	if err := v.checkRevoked(ctx, cl, strict); err != nil {
		return nil, err
	}
	return cl, nil
```

Create `jwtverify/revocation.go`:

```go
package jwtverify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"gopkg.aoctech.app/api-commons/cache"
)

// Revocation list for account lock/deletion. ctech-account writes an entry when
// it locks a user; every service's Verifier rejects that user's tokens issued
// at or before the cut-off. Entries only need to outlive the access-token TTL:
// refresh tokens are revoked server-side, so no newer token can be minted.
//
// The entries live in Valkey DB 0 (the shared base URL). Services whose main
// cache uses another logical DB must pass a backend built on the base URL.
const revokedSubPrefix = "ctech:jwt:revoked_sub:"

// RevocationTTL is the 15-minute access-token lifetime plus clock skew.
const RevocationTTL = 20 * time.Minute

var (
	ErrTokenRevoked          = errors.New("jwtverify: token revoked")
	ErrRevocationUnavailable = errors.New("jwtverify: revocation list unavailable")
)

// Revoke rejects every token of sub issued at or before cutoff, for ttl.
func Revoke(ctx context.Context, c cache.Backend, sub string, cutoff time.Time, ttl time.Duration) error {
	return c.Set(ctx, revokedSubPrefix+sub, []byte(strconv.FormatInt(cutoff.Unix(), 10)), int(ttl.Seconds()))
}

// Unrevoke removes sub's entry (deletion request cancelled during grace).
func Unrevoke(ctx context.Context, c cache.Backend, sub string) error {
	return c.Delete(ctx, revokedSubPrefix+sub)
}

// WithRevocation enables the revocation check against c and returns v.
func (v *Verifier) WithRevocation(c cache.Backend) *Verifier {
	v.revocation = c
	return v
}

func (v *Verifier) checkRevoked(ctx context.Context, cl *Claims, strict bool) error {
	if v.revocation == nil {
		return nil
	}
	raw, ok, err := v.revocation.Get(ctx, revokedSubPrefix+cl.Sub)
	if err != nil {
		if strict {
			return fmt.Errorf("%w: %v", ErrRevocationUnavailable, err)
		}
		slog.WarnContext(ctx, "jwtverify: revocation check skipped", "error", err)
		return nil
	}
	if !ok {
		return nil
	}
	cutoff, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || cl.IssuedAt <= cutoff {
		return ErrTokenRevoked // an unparseable entry fails safe
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./jwtverify/ -race -count=1`
Expected: PASS (new tests and every existing `verifier_test.go` test).

- [ ] **Step 5: Commit**

```bash
git add jwtverify/
git commit -m "feat(jwtverify): revocation list for locked accounts (fail-open + strict)"
```

---

### Task 2: `erasure` message contract

**Files:**
- Create: `erasure/message.go`
- Test: `erasure/message_test.go`

**Interfaces:**
- Produces:
  - `const Version = 1`
  - `type Type string` with `TypeLocked = "user.locked"`, `TypeUnlocked = "user.unlocked"`, `TypeErase = "user.erase"`
  - `type Scope string` with `ScopeAccount = "account"`, `ScopeService = "service"`
  - `type Message struct { Version int; Type Type; RequestID, Sub string; Scope Scope; Services, Organizations []string; Attempt int; IssuedAt time.Time }` (JSON tags below)
  - `var ErrInvalidMessage error`
  - `func (m Message) Validate() error`, `func (m Message) Targets(service string) bool`
  - `func Encode(m Message) ([]byte, error)`, `func Decode(body []byte) (Message, error)`
  - `type Blocker struct { Code string; Detail map[string]any; ActionURL string }`
  - `type Eligibility struct { Eligible bool; Blockers []Blocker }`, `func NewEligibility(blockers ...Blocker) Eligibility`
  - `type Result string` with `ResultDone = "done"`, `ResultBlocked = "blocked"`
  - `type Ack struct { RequestID, Service string; Result Result; Counts map[string]int; Blockers []Blocker; At time.Time }`

- [ ] **Step 1: Write the failing tests**

Create `erasure/message_test.go`:

```go
package erasure

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func validMessage() Message {
	return Message{
		Version:   Version,
		Type:      TypeErase,
		RequestID: "01JREQ",
		Sub:       "user-1",
		Scope:     ScopeAccount,
		Services:  []string{"wallet", "dfe"},
		Attempt:   1,
		IssuedAt:  time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC),
	}
}

func TestEncodeDecode_RoundTrip(t *testing.T) {
	in := validMessage()
	in.Organizations = []string{"org-1"}
	raw, err := Encode(in)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	out, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if out.RequestID != in.RequestID || out.Sub != in.Sub || !out.IssuedAt.Equal(in.IssuedAt) ||
		len(out.Organizations) != 1 || out.Organizations[0] != "org-1" {
		t.Fatalf("round trip mismatch: %+v", out)
	}
}

func TestDecode_SNSEnvelope(t *testing.T) {
	raw, _ := Encode(validMessage())
	env, _ := json.Marshal(map[string]string{"Type": "Notification", "MessageId": "m-1", "Message": string(raw)})
	out, err := Decode(env)
	if err != nil {
		t.Fatalf("Decode(envelope): %v", err)
	}
	if out.Sub != "user-1" || out.Type != TypeErase {
		t.Fatalf("unexpected message: %+v", out)
	}
}

func TestDecode_RejectsInvalid(t *testing.T) {
	cases := map[string]func(m *Message){
		"version":            func(m *Message) { m.Version = 2 },
		"type":               func(m *Message) { m.Type = "user.deleted" },
		"request_id":         func(m *Message) { m.RequestID = "" },
		"sub":                func(m *Message) { m.Sub = "" },
		"scope":              func(m *Message) { m.Scope = "tenant" },
		"services":           func(m *Message) { m.Services = nil },
		"issued_at":          func(m *Message) { m.IssuedAt = time.Time{} },
		"orgs with service":  func(m *Message) { m.Scope = ScopeService; m.Organizations = []string{"org-1"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := validMessage()
			mutate(&m)
			raw, _ := json.Marshal(m)
			if _, err := Decode(raw); !errors.Is(err, ErrInvalidMessage) {
				t.Fatalf("Decode err = %v, want ErrInvalidMessage", err)
			}
			if _, err := Encode(m); !errors.Is(err, ErrInvalidMessage) {
				t.Fatalf("Encode err = %v, want ErrInvalidMessage", err)
			}
		})
	}
	if _, err := Decode([]byte("{not json")); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("malformed JSON err = %v, want ErrInvalidMessage", err)
	}
}

func TestTargets(t *testing.T) {
	m := validMessage()
	if !m.Targets("wallet") || m.Targets("poker") {
		t.Fatalf("Targets wrong for %v", m.Services)
	}
}

func TestNewEligibility_EmptyBlockersIsEligibleArray(t *testing.T) {
	raw, _ := json.Marshal(NewEligibility())
	if !strings.Contains(string(raw), `"eligible":true`) || !strings.Contains(string(raw), `"blockers":[]`) {
		t.Fatalf("unexpected JSON: %s", raw)
	}
	e := NewEligibility(Blocker{Code: "wallet.balance_nonzero"})
	if e.Eligible || len(e.Blockers) != 1 {
		t.Fatalf("unexpected eligibility: %+v", e)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./erasure/ -count=1`
Expected: build FAIL: `undefined: Message`, `undefined: Encode`, ...

- [ ] **Step 3: Implement**

Create `erasure/message.go`:

```go
// Package erasure is the participant side of the CTech account-deletion saga
// (ctech-account docs/specs/2026-10-06-account-deletion-saga-protocol.md):
// the message contract, per-service lock/tombstone state, the SQS consumer and
// the ack client. Orchestration lives in ctech-account, not here.
package erasure

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Version is the message schema version this package reads and writes.
const Version = 1

type Type string

const (
	TypeLocked   Type = "user.locked"
	TypeUnlocked Type = "user.unlocked"
	TypeErase    Type = "user.erase"
)

type Scope string

const (
	ScopeAccount Scope = "account" // full account deletion
	ScopeService Scope = "service" // the user leaves one service only
)

// Message is published by ctech-account on the user-erasure SNS topic. It never
// carries personal data beyond the opaque account sub.
type Message struct {
	Version       int       `json:"version"`
	Type          Type      `json:"type"`
	RequestID     string    `json:"request_id"`
	Sub           string    `json:"sub"`
	Scope         Scope     `json:"scope"`
	Services      []string  `json:"services"`
	Organizations []string  `json:"organizations,omitempty"` // single-member orgs erased with the user; account scope only
	Attempt       int       `json:"attempt"`
	IssuedAt      time.Time `json:"issued_at"` // orders lock/unlock; SQS does not
}

var ErrInvalidMessage = errors.New("erasure: invalid message")

func (m Message) Validate() error {
	var problem string
	switch {
	case m.Version != Version:
		problem = fmt.Sprintf("unsupported version %d", m.Version)
	case m.Type != TypeLocked && m.Type != TypeUnlocked && m.Type != TypeErase:
		problem = fmt.Sprintf("unknown type %q", m.Type)
	case m.RequestID == "":
		problem = "missing request_id"
	case m.Sub == "":
		problem = "missing sub"
	case m.Scope != ScopeAccount && m.Scope != ScopeService:
		problem = fmt.Sprintf("unknown scope %q", m.Scope)
	case len(m.Services) == 0:
		problem = "no target services"
	case m.Scope == ScopeService && len(m.Organizations) > 0:
		problem = "organizations require scope account"
	case m.IssuedAt.IsZero():
		problem = "missing issued_at"
	default:
		return nil
	}
	return fmt.Errorf("%w: %s", ErrInvalidMessage, problem)
}

// Targets reports whether service is one of the message's recipients. The SNS
// filter policy already routes by services; this is the consumer-side check.
func (m Message) Targets(service string) bool { return slices.Contains(m.Services, service) }

func Encode(m Message) ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(m)
}

// Decode parses a queue body. It accepts both raw delivery and the SNS
// envelope, so a subscription created without RawMessageDelivery still works
// instead of sending every message to the DLQ.
func Decode(body []byte) (Message, error) {
	var env struct {
		Type    string `json:"Type"`
		Message string `json:"Message"`
	}
	if json.Unmarshal(body, &env) == nil && env.Type == "Notification" {
		body = []byte(env.Message)
	}
	var m Message
	if err := json.Unmarshal(body, &m); err != nil {
		return Message{}, fmt.Errorf("%w: %v", ErrInvalidMessage, err)
	}
	if err := m.Validate(); err != nil {
		return Message{}, err
	}
	return m, nil
}

// Blocker is one reason a user cannot be erased yet. Code is stable and
// translated by the account UI (e.g. "wallet.balance_nonzero").
type Blocker struct {
	Code      string         `json:"code"`
	Detail    map[string]any `json:"detail,omitempty"`
	ActionURL string         `json:"action_url,omitempty"`
}

// Eligibility is the body of GET /internal/erasure/eligibility/{sub}.
type Eligibility struct {
	Eligible bool      `json:"eligible"`
	Blockers []Blocker `json:"blockers"`
}

// NewEligibility is eligible exactly when there are no blockers; Blockers is
// never nil so it serializes as [].
func NewEligibility(blockers ...Blocker) Eligibility {
	return Eligibility{Eligible: len(blockers) == 0, Blockers: append([]Blocker{}, blockers...)}
}

type Result string

const (
	ResultDone    Result = "done"
	ResultBlocked Result = "blocked" // a blocker appeared; nothing was erased
)

// Ack is POSTed to ctech-account's /internal/erasure/ack after a user.erase.
type Ack struct {
	RequestID string         `json:"request_id"`
	Service   string         `json:"service"`
	Result    Result         `json:"result"`
	Counts    map[string]int `json:"counts,omitempty"` // per store: erased/anonymized/retained
	Blockers  []Blocker      `json:"blockers,omitempty"`
	At        time.Time      `json:"at"`
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./erasure/ -race -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add erasure/message.go erasure/message_test.go
git commit -m "feat(erasure): account-deletion message contract"
```

---

### Task 3: `erasure` state store (lock + tombstone)

**Files:**
- Create: `erasure/state.go`
- Test: `erasure/state_test.go`

**Interfaces:**
- Consumes: `Message`, `TypeLocked/TypeUnlocked/TypeErase` (Task 2); `dynamo.TableName`, `dynamo.IsConditionFailed`.
- Produces:
  - `const TableSuffix = "erasure_state"`
  - `type State string` with `StateActive`, `StateLocked`, `StateErased`
  - `type Record struct { PK string; State State; RequestID string; SeqNS int64; UpdatedAt string; TTL int64 }`
  - `func SubKey(sub string) string` → `"SUB#"+sub`; `func OrgKey(orgID string) string` → `"ORG#"+orgID`
  - `var ErrConcurrentUpdate error`
  - `func NewStore(db dynamoAPI, tablePrefix string, erasedTTL time.Duration) *Store` (`*dynamodb.Client` satisfies `dynamoAPI`)
  - `func (s *Store) Get(ctx context.Context, key string) (*Record, error)`
  - `func (s *Store) Apply(ctx context.Context, key string, m Message) (Record, error)`
  - `func (s *Store) Blocked(ctx context.Context, sub string) (bool, error)`
  - `func (s *Store) OrgErased(ctx context.Context, orgID string) (bool, error)`
  - `func (s *Store) Clear(ctx context.Context, sub string) error`

- [ ] **Step 1: Write the failing tests**

Create `erasure/state_test.go`:

```go
package erasure

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// fakeDynamo honours exactly the condition expressions Store uses.
type fakeDynamo struct {
	mu    sync.Mutex
	items map[string]map[string]types.AttributeValue
	racer func(f *fakeDynamo) // runs once, inside the next PutItem, before the condition check
}

func newFakeDynamo() *fakeDynamo {
	return &fakeDynamo{items: map[string]map[string]types.AttributeValue{}}
}

func pkOf(m map[string]types.AttributeValue) string { return attrS(m["pk"]) }

func attrS(v types.AttributeValue) string {
	if s, ok := v.(*types.AttributeValueMemberS); ok {
		return s.Value
	}
	return ""
}

func attrN(v types.AttributeValue) string {
	if n, ok := v.(*types.AttributeValueMemberN); ok {
		return n.Value
	}
	return ""
}

func condFailed() error {
	return &types.ConditionalCheckFailedException{Message: aws.String("conditional check failed")}
}

func (f *fakeDynamo) GetItem(_ context.Context, in *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &dynamodb.GetItemOutput{Item: f.items[pkOf(in.Key)]}, nil
}

func (f *fakeDynamo) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	if r := f.racer; r != nil {
		f.racer = nil
		r(f)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := pkOf(in.Item)
	cur, exists := f.items[key]
	switch cond := aws.ToString(in.ConditionExpression); cond {
	case "attribute_not_exists(pk)":
		if exists {
			return nil, condFailed()
		}
	case "seq_ns = :old_seq AND erasure_state = :old_state":
		v := in.ExpressionAttributeValues
		if !exists || attrN(cur["seq_ns"]) != attrN(v[":old_seq"]) || attrS(cur["erasure_state"]) != attrS(v[":old_state"]) {
			return nil, condFailed()
		}
	default:
		return nil, fmt.Errorf("fakeDynamo: unsupported condition %q", cond)
	}
	f.items[key] = in.Item
	return &dynamodb.PutItemOutput{}, nil
}

func (f *fakeDynamo) DeleteItem(_ context.Context, in *dynamodb.DeleteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := pkOf(in.Key)
	cur, exists := f.items[key]
	if cond := aws.ToString(in.ConditionExpression); cond != "erasure_state = :erased" {
		return nil, fmt.Errorf("fakeDynamo: unsupported condition %q", cond)
	}
	if !exists || attrS(cur["erasure_state"]) != string(StateErased) {
		return nil, condFailed()
	}
	delete(f.items, key)
	return &dynamodb.DeleteItemOutput{}, nil
}

var t0 = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func msgAt(typ Type, req string, offset time.Duration) Message {
	m := validMessage()
	m.Type, m.RequestID, m.IssuedAt = typ, req, t0.Add(offset)
	return m
}

func TestApply_LockThenUnlock(t *testing.T) {
	r, changed := apply(nil, "SUB#u", msgAt(TypeLocked, "r1", 0), t0, 0)
	if !changed || r.State != StateLocked || r.RequestID != "r1" {
		t.Fatalf("after lock: %+v changed=%v", r, changed)
	}
	r, changed = apply(&r, "SUB#u", msgAt(TypeUnlocked, "r1", time.Second), t0, 0)
	if !changed || r.State != StateActive || r.TTL == 0 {
		t.Fatalf("after unlock: %+v changed=%v (active records must carry a ttl)", r, changed)
	}
}

func TestApply_UnlockBeforeLock_StaleLockIgnored(t *testing.T) {
	r, _ := apply(nil, "SUB#u", msgAt(TypeUnlocked, "r1", time.Second), t0, 0)
	r, changed := apply(&r, "SUB#u", msgAt(TypeLocked, "r1", 0), t0, 0)
	if changed || r.State != StateActive {
		t.Fatalf("late lock must be ignored: %+v changed=%v", r, changed)
	}
}

func TestApply_DuplicateLockIsNoop(t *testing.T) {
	m := msgAt(TypeLocked, "r1", 0)
	r, _ := apply(nil, "SUB#u", m, t0, 0)
	if _, changed := apply(&r, "SUB#u", m, t0, 0); changed {
		t.Fatal("redelivered lock must not change the record")
	}
}

func TestApply_EraseIsTerminal(t *testing.T) {
	r, _ := apply(nil, "SUB#u", msgAt(TypeErase, "r1", 0), t0, 0)
	if r.State != StateErased {
		t.Fatalf("state = %s, want erased", r.State)
	}
	for _, typ := range []Type{TypeLocked, TypeUnlocked, TypeErase} {
		if _, changed := apply(&r, "SUB#u", msgAt(typ, "r2", time.Hour), t0, 0); changed {
			t.Fatalf("%s changed an erased record", typ)
		}
	}
}

func TestApply_ErasedTTL(t *testing.T) {
	r, _ := apply(nil, "SUB#u", msgAt(TypeErase, "r1", 0), t0, 0)
	if r.TTL != 0 {
		t.Fatalf("erasedTTL 0 must keep the tombstone forever, got ttl %d", r.TTL)
	}
	r, _ = apply(nil, "SUB#u", msgAt(TypeErase, "r1", 0), t0, 24*time.Hour)
	if want := t0.Add(24 * time.Hour).Unix(); r.TTL != want {
		t.Fatalf("ttl = %d, want %d", r.TTL, want)
	}
}

func TestStore_ApplyPersistsAndBlocked(t *testing.T) {
	ctx := context.Background()
	s := NewStore(newFakeDynamo(), "test", 0)
	if s.table != "test_erasure_state" {
		t.Fatalf("table = %q", s.table)
	}
	if blocked, err := s.Blocked(ctx, "u"); err != nil || blocked {
		t.Fatalf("unknown sub: blocked=%v err=%v", blocked, err)
	}
	if _, err := s.Apply(ctx, SubKey("u"), msgAt(TypeLocked, "r1", 0)); err != nil {
		t.Fatalf("Apply lock: %v", err)
	}
	if blocked, _ := s.Blocked(ctx, "u"); !blocked {
		t.Fatal("locked sub must be blocked")
	}
	if _, err := s.Apply(ctx, SubKey("u"), msgAt(TypeUnlocked, "r1", time.Second)); err != nil {
		t.Fatalf("Apply unlock: %v", err)
	}
	if blocked, _ := s.Blocked(ctx, "u"); blocked {
		t.Fatal("unlocked sub must not be blocked")
	}
}

func TestStore_ConcurrentChangeRetries(t *testing.T) {
	ctx := context.Background()
	f := newFakeDynamo()
	s := NewStore(f, "test", 0)
	// Between our read (no record) and our write, another consumer stores a newer lock.
	f.racer = func(f *fakeDynamo) {
		f.items["SUB#u"] = map[string]types.AttributeValue{
			"pk":            &types.AttributeValueMemberS{Value: "SUB#u"},
			"erasure_state": &types.AttributeValueMemberS{Value: string(StateLocked)},
			"request_id":    &types.AttributeValueMemberS{Value: "r2"},
			"seq_ns":        &types.AttributeValueMemberN{Value: strconv.FormatInt(t0.Add(time.Hour).UnixNano(), 10)},
		}
	}
	r, err := s.Apply(ctx, SubKey("u"), msgAt(TypeUnlocked, "r1", 0))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if r.State != StateLocked || r.RequestID != "r2" {
		t.Fatalf("stale unlock overwrote a newer lock: %+v", r)
	}
}

func TestStore_OrgErasedAndClear(t *testing.T) {
	ctx := context.Background()
	s := NewStore(newFakeDynamo(), "test", 0)
	_, _ = s.Apply(ctx, OrgKey("org-1"), msgAt(TypeErase, "r1", 0))
	if erased, err := s.OrgErased(ctx, "org-1"); err != nil || !erased {
		t.Fatalf("OrgErased = %v, %v", erased, err)
	}

	_, _ = s.Apply(ctx, SubKey("locked"), msgAt(TypeLocked, "r1", 0))
	if err := s.Clear(ctx, "locked"); err != nil {
		t.Fatalf("Clear on a locked record must be a no-op, got %v", err)
	}
	if blocked, _ := s.Blocked(ctx, "locked"); !blocked {
		t.Fatal("Clear must not remove a lock")
	}

	_, _ = s.Apply(ctx, SubKey("gone"), msgAt(TypeErase, "r1", 0))
	if err := s.Clear(ctx, "gone"); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if blocked, _ := s.Blocked(ctx, "gone"); blocked {
		t.Fatal("cleared tombstone must not block a returning user")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./erasure/ -run 'Apply|Store' -count=1`
Expected: build FAIL: `undefined: apply`, `undefined: NewStore`, ...

- [ ] **Step 3: Implement**

Create `erasure/state.go`:

```go
package erasure

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"gopkg.aoctech.app/api-commons/dynamo"
)

// TableSuffix names each participant's state table, {prefix}_erasure_state:
// partition key "pk" (S) only, TTL attribute "ttl". Each service's infra creates it.
const TableSuffix = "erasure_state"

// activeMemory keeps an unlocked record long enough that a lock delivered late
// (older issued_at) is still recognised as stale.
const activeMemory = 30 * 24 * time.Hour

type State string

const (
	StateActive State = "active"
	StateLocked State = "locked" // refuse state-changing operations
	StateErased State = "erased" // terminal tombstone: drop any work for this key
)

type Record struct {
	PK        string `dynamodbav:"pk"`
	State     State  `dynamodbav:"erasure_state"`
	RequestID string `dynamodbav:"request_id"`
	SeqNS     int64  `dynamodbav:"seq_ns"` // issued_at (ns) of the newest message applied
	UpdatedAt string `dynamodbav:"updated_at"`
	TTL       int64  `dynamodbav:"ttl,omitempty"`
}

func SubKey(sub string) string   { return "SUB#" + sub }
func OrgKey(orgID string) string { return "ORG#" + orgID }

var ErrConcurrentUpdate = errors.New("erasure: state changed concurrently, retries exhausted")

type dynamoAPI interface {
	GetItem(ctx context.Context, in *dynamodb.GetItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	PutItem(ctx context.Context, in *dynamodb.PutItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	DeleteItem(ctx context.Context, in *dynamodb.DeleteItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
}

type Store struct {
	db        dynamoAPI
	table     string
	erasedTTL time.Duration
	now       func() time.Time
}

// NewStore binds the service's erasure-state table. erasedTTL is how long a
// tombstone is kept (the service's longest retention); 0 keeps it forever.
func NewStore(db dynamoAPI, tablePrefix string, erasedTTL time.Duration) *Store {
	return &Store{db: db, table: dynamo.TableName(tablePrefix, TableSuffix), erasedTTL: erasedTTL, now: time.Now}
}

// apply is the whole state machine. Lock/unlock are ordered by issued_at
// because SQS is not; erase always wins and is terminal.
func apply(cur *Record, key string, m Message, now time.Time, erasedTTL time.Duration) (Record, bool) {
	prev := Record{PK: key, State: StateActive}
	if cur != nil {
		prev = *cur
	}
	if prev.State == StateErased {
		return prev, false
	}
	seq := m.IssuedAt.UnixNano()
	next := Record{PK: key, RequestID: m.RequestID, SeqNS: max(prev.SeqNS, seq), UpdatedAt: now.UTC().Format(time.RFC3339)}
	switch m.Type {
	case TypeErase:
		next.State = StateErased
		if erasedTTL > 0 {
			next.TTL = now.Add(erasedTTL).Unix()
		}
		return next, true
	case TypeLocked, TypeUnlocked:
		if seq <= prev.SeqNS {
			return prev, false
		}
		next.State = StateLocked
		if m.Type == TypeUnlocked {
			next.State = StateActive
			next.TTL = now.Add(activeMemory).Unix()
		}
		return next, true
	}
	return prev, false
}

func (s *Store) Get(ctx context.Context, key string) (*Record, error) {
	out, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(s.table),
		Key:            map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: key}},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return nil, fmt.Errorf("erasure: get %s: %w", key, err)
	}
	if len(out.Item) == 0 {
		return nil, nil
	}
	var r Record
	if err := attributevalue.UnmarshalMap(out.Item, &r); err != nil {
		return nil, fmt.Errorf("erasure: decode %s: %w", key, err)
	}
	return &r, nil
}

// Apply moves the record at key according to m. Idempotent: redelivering the
// same message is a no-op. Uses optimistic concurrency against other consumers.
func (s *Store) Apply(ctx context.Context, key string, m Message) (Record, error) {
	for range 3 {
		cur, err := s.Get(ctx, key)
		if err != nil {
			return Record{}, err
		}
		next, changed := apply(cur, key, m, s.now(), s.erasedTTL)
		if !changed {
			return next, nil
		}
		err = s.put(ctx, next, cur)
		if dynamo.IsConditionFailed(err) {
			continue
		}
		if err != nil {
			return Record{}, fmt.Errorf("erasure: put %s: %w", key, err)
		}
		return next, nil
	}
	return Record{}, ErrConcurrentUpdate
}

func (s *Store) put(ctx context.Context, next Record, cur *Record) error {
	item, err := attributevalue.MarshalMap(next)
	if err != nil {
		return err
	}
	in := &dynamodb.PutItemInput{TableName: aws.String(s.table), Item: item}
	if cur == nil {
		in.ConditionExpression = aws.String("attribute_not_exists(pk)")
	} else {
		in.ConditionExpression = aws.String("seq_ns = :old_seq AND erasure_state = :old_state")
		in.ExpressionAttributeValues = map[string]types.AttributeValue{
			":old_seq":   &types.AttributeValueMemberN{Value: strconv.FormatInt(cur.SeqNS, 10)},
			":old_state": &types.AttributeValueMemberS{Value: string(cur.State)},
		}
	}
	_, err = s.db.PutItem(ctx, in)
	return err
}

// Blocked reports whether sub must be refused state-changing operations
// (locked or erased). Services call it on every write path for a user.
func (s *Store) Blocked(ctx context.Context, sub string) (bool, error) {
	r, err := s.Get(ctx, SubKey(sub))
	return r != nil && r.State != StateActive, err
}

// OrgErased reports whether an organization was erased with its only member.
func (s *Store) OrgErased(ctx context.Context, orgID string) (bool, error) {
	r, err := s.Get(ctx, OrgKey(orgID))
	return r != nil && r.State == StateErased, err
}

// Clear removes sub's tombstone when a user who left this service (scope
// service) consents to it again. A record that is not erased is left alone.
func (s *Store) Clear(ctx context.Context, sub string) error {
	_, err := s.db.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName:                 aws.String(s.table),
		Key:                       map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: SubKey(sub)}},
		ConditionExpression:       aws.String("erasure_state = :erased"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":erased": &types.AttributeValueMemberS{Value: string(StateErased)}},
	})
	if dynamo.IsConditionFailed(err) {
		return nil
	}
	return err
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./erasure/ -race -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add erasure/state.go erasure/state_test.go
git commit -m "feat(erasure): ordered lock/tombstone state store"
```

---

### Task 4: `erasure` ack client

**Files:**
- Create: `erasure/ack.go`
- Test: `erasure/ack_test.go`

**Interfaces:**
- Consumes: `Ack` (Task 2); `oauth2client.New(httpClient *http.Client, cache cache.Backend, tokenURL, clientID, clientSecret, scope string) *TokenManager`.
- Produces:
  - `func NewAckClient(httpClient *http.Client, ackURL string, tokens *oauth2client.TokenManager) *AckClient`
  - `func (c *AckClient) Send(ctx context.Context, a Ack) error`
  - test helper `newAckServer(t) *ackServer` (fields `srv *httptest.Server`, `status atomic.Int64`, `mu sync.Mutex`, `acks []Ack`; methods `client() *AckClient`, `received() []Ack`), reused by Task 5.

- [ ] **Step 1: Write the failing tests**

Create `erasure/ack_test.go`:

```go
package erasure

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.aoctech.app/api-commons/oauth2client"
)

// ackServer fakes ctech-account: a client_credentials token endpoint and the ack endpoint.
type ackServer struct {
	srv    *httptest.Server
	status atomic.Int64
	mu     sync.Mutex
	acks   []Ack
	auth   []string
}

func newAckServer(t *testing.T) *ackServer {
	t.Helper()
	a := &ackServer{}
	a.status.Store(http.StatusNoContent)
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","token_type":"Bearer","expires_in":3600}`))
	})
	mux.HandleFunc("/ack", func(w http.ResponseWriter, r *http.Request) {
		code := int(a.status.Load())
		if code/100 == 2 {
			var ack Ack
			_ = json.NewDecoder(r.Body).Decode(&ack)
			a.mu.Lock()
			a.acks = append(a.acks, ack)
			a.auth = append(a.auth, r.Header.Get("Authorization"))
			a.mu.Unlock()
		}
		w.WriteHeader(code)
		if code/100 != 2 {
			_, _ = w.Write([]byte(`{"title":"boom"}`))
		}
	})
	a.srv = httptest.NewServer(mux)
	t.Cleanup(a.srv.Close)
	return a
}

func (a *ackServer) client() *AckClient {
	tm := oauth2client.New(a.srv.Client(), nil, a.srv.URL+"/token", "dfe", "secret", "account:erasure:ack")
	return NewAckClient(a.srv.Client(), a.srv.URL+"/ack", tm)
}

func (a *ackServer) received() []Ack {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Ack(nil), a.acks...)
}

func TestAckClient_SendsBearerJSON(t *testing.T) {
	a := newAckServer(t)
	in := Ack{RequestID: "r1", Service: "dfe", Result: ResultDone, Counts: map[string]int{"users": 1}, At: t0}
	if err := a.client().Send(context.Background(), in); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got := a.received()
	if len(got) != 1 || got[0].RequestID != "r1" || got[0].Counts["users"] != 1 || !got[0].At.Equal(t0) {
		t.Fatalf("unexpected acks: %+v", got)
	}
	if a.auth[0] != "Bearer tok" {
		t.Fatalf("Authorization = %q", a.auth[0])
	}
}

func TestAckClient_Non2xxIsError(t *testing.T) {
	a := newAckServer(t)
	a.status.Store(http.StatusInternalServerError)
	err := a.client().Send(context.Background(), Ack{RequestID: "r1", Service: "dfe", Result: ResultDone, At: time.Now()})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v, want a 500 error", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./erasure/ -run AckClient -count=1`
Expected: build FAIL: `undefined: NewAckClient`.

- [ ] **Step 3: Implement**

Create `erasure/ack.go`:

```go
package erasure

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"gopkg.aoctech.app/api-commons/oauth2client"
)

// AckClient reports a finished user.erase to ctech-account with the service's
// client_credentials token (scope account:erasure:ack).
type AckClient struct {
	http   *http.Client
	url    string
	tokens *oauth2client.TokenManager
}

func NewAckClient(httpClient *http.Client, ackURL string, tokens *oauth2client.TokenManager) *AckClient {
	return &AckClient{http: httpClient, url: ackURL, tokens: tokens}
}

// Send POSTs a. Any non-2xx is an error, so the consumer leaves the message
// for redelivery and the (idempotent) purge runs again.
func (c *AckClient) Send(ctx context.Context, a Ack) error {
	body, err := json.Marshal(a)
	if err != nil {
		return err
	}
	token, err := c.tokens.Get(ctx)
	if err != nil {
		return fmt.Errorf("erasure: ack token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("erasure: ack %s: %w", a.RequestID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("erasure: ack %s rejected: status %d: %s", a.RequestID, resp.StatusCode, msg)
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./erasure/ -race -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add erasure/ack.go erasure/ack_test.go
git commit -m "feat(erasure): ack client for ctech-account"
```

---

### Task 5: `erasure` SQS consumer

**Files:**
- Modify: `go.mod`, `go.sum` (add `github.com/aws/aws-sdk-go-v2/service/sqs`)
- Create: `erasure/consumer.go`
- Test: `erasure/consumer_test.go`

**Interfaces:**
- Consumes: `Decode`, `Message.Targets`, `Ack`, `ResultDone/ResultBlocked` (Task 2); `Store.Apply`, `SubKey`, `OrgKey` (Task 3); `AckClient.Send` and test helper `newAckServer` (Task 4); test helpers `newFakeDynamo`, `msgAt`, `validMessage`, `t0` (Tasks 2–3).
- Produces:
  - `type PurgeFunc func(ctx context.Context, m Message) (Ack, error)`: the service's purge. It returns `Result` `done` or `blocked` (+ `Counts`/`Blockers`); the consumer fills `RequestID`, `Service`, `At`.
  - `func NewConsumer(sqsClient sqsAPI, queueURL, service string, store *Store, purge PurgeFunc, acks *AckClient) *Consumer` (`*sqs.Client` satisfies `sqsAPI`)
  - `func (c *Consumer) Run(ctx context.Context) error`

- [ ] **Step 1: Add the dependency**

Run: `go get github.com/aws/aws-sdk-go-v2/service/sqs && go mod tidy`
Expected: `go.mod` gains `github.com/aws/aws-sdk-go-v2/service/sqs` in the main `require` block.

- [ ] **Step 2: Write the failing tests**

Create `erasure/consumer_test.go`:

```go
package erasure

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

type purgeRecorder struct {
	mu    sync.Mutex
	calls int
	ack   Ack
	err   error
}

func (p *purgeRecorder) fn(_ context.Context, _ Message) (Ack, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return p.ack, p.err
}

func newTestConsumer(t *testing.T, service string, p *purgeRecorder) (*Consumer, *Store, *ackServer) {
	t.Helper()
	store := NewStore(newFakeDynamo(), "test", 0)
	a := newAckServer(t)
	return NewConsumer(nil, "queue", service, store, p.fn, a.client()), store, a
}

func body(t *testing.T, m Message) string {
	t.Helper()
	raw, err := Encode(m)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return string(raw)
}

func TestConsumer_LockAndUnlock(t *testing.T) {
	ctx := context.Background()
	c, store, _ := newTestConsumer(t, "wallet", &purgeRecorder{})
	if err := c.handle(ctx, body(t, msgAt(TypeLocked, "r1", 0))); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if blocked, _ := store.Blocked(ctx, "user-1"); !blocked {
		t.Fatal("user must be blocked after user.locked")
	}
	if err := c.handle(ctx, body(t, msgAt(TypeUnlocked, "r1", 1))); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if blocked, _ := store.Blocked(ctx, "user-1"); blocked {
		t.Fatal("user must be active after user.unlocked")
	}
}

func TestConsumer_EraseDone(t *testing.T) {
	ctx := context.Background()
	p := &purgeRecorder{ack: Ack{Result: ResultDone, Counts: map[string]int{"users": 1}}}
	c, store, a := newTestConsumer(t, "dfe", p)
	m := msgAt(TypeErase, "r1", 0)
	m.Organizations = []string{"org-1"}
	if err := c.handle(ctx, body(t, m)); err != nil {
		t.Fatalf("erase: %v", err)
	}
	if r, _ := store.Get(ctx, SubKey("user-1")); r == nil || r.State != StateErased {
		t.Fatalf("sub state = %+v, want erased", r)
	}
	if erased, _ := store.OrgErased(ctx, "org-1"); !erased {
		t.Fatal("organization must be tombstoned")
	}
	acks := a.received()
	if len(acks) != 1 || acks[0].RequestID != "r1" || acks[0].Service != "dfe" || acks[0].Result != ResultDone || acks[0].At.IsZero() {
		t.Fatalf("unexpected acks: %+v", acks)
	}
}

func TestConsumer_EraseBlocked_LocksWithoutErasing(t *testing.T) {
	ctx := context.Background()
	p := &purgeRecorder{ack: Ack{Result: ResultBlocked, Blockers: []Blocker{{Code: "wallet.balance_nonzero"}}}}
	c, store, a := newTestConsumer(t, "wallet", p)
	// No user.locked was ever delivered for this sub.
	if err := c.handle(ctx, body(t, msgAt(TypeErase, "r1", 0))); err != nil {
		t.Fatalf("erase: %v", err)
	}
	if r, _ := store.Get(ctx, SubKey("user-1")); r == nil || r.State != StateLocked {
		t.Fatalf("sub state = %+v, want locked", r)
	}
	if acks := a.received(); len(acks) != 1 || acks[0].Result != ResultBlocked || len(acks[0].Blockers) != 1 {
		t.Fatalf("unexpected acks: %+v", acks)
	}
}

func TestConsumer_PurgeErrorSendsNoAck(t *testing.T) {
	ctx := context.Background()
	c, store, a := newTestConsumer(t, "wallet", &purgeRecorder{err: errors.New("dynamo throttled")})
	if err := c.handle(ctx, body(t, msgAt(TypeErase, "r1", 0))); err == nil {
		t.Fatal("purge error must surface so the message is redelivered")
	}
	if len(a.received()) != 0 {
		t.Fatal("no ack may be sent for a failed purge")
	}
	if r, _ := store.Get(ctx, SubKey("user-1")); r == nil || r.State != StateLocked {
		t.Fatalf("sub state = %+v, want locked", r)
	}
}

func TestConsumer_AckFailureRedelivers(t *testing.T) {
	ctx := context.Background()
	p := &purgeRecorder{ack: Ack{Result: ResultDone}}
	c, _, a := newTestConsumer(t, "dfe", p) // validMessage targets wallet and dfe
	b := body(t, msgAt(TypeErase, "r1", 0))

	a.status.Store(http.StatusServiceUnavailable)
	if err := c.handle(ctx, b); err == nil {
		t.Fatal("ack failure must surface")
	}
	a.status.Store(http.StatusNoContent)
	if err := c.handle(ctx, b); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if p.calls != 2 {
		t.Fatalf("purge calls = %d, want 2 (re-run on redelivery)", p.calls)
	}
	if acks := a.received(); len(acks) != 1 || acks[0].Result != ResultDone {
		t.Fatalf("unexpected acks: %+v", acks)
	}
}

func TestConsumer_IgnoresOtherService(t *testing.T) {
	p := &purgeRecorder{ack: Ack{Result: ResultDone}}
	c, _, _ := newTestConsumer(t, "billing", p)
	if err := c.handle(context.Background(), body(t, msgAt(TypeErase, "r1", 0))); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if p.calls != 0 {
		t.Fatal("purge must not run for a message not addressed to this service")
	}
}

func TestConsumer_InvalidResultIsError(t *testing.T) {
	c, _, _ := newTestConsumer(t, "wallet", &purgeRecorder{ack: Ack{}})
	if err := c.handle(context.Background(), body(t, msgAt(TypeErase, "r1", 0))); err == nil {
		t.Fatal("an empty Result must be rejected")
	}
}

// fakeSQS serves its messages once per receive and cancels the run after maxReceives.
type fakeSQS struct {
	mu          sync.Mutex
	msgs        []sqstypes.Message
	deleted     []string
	receives    int
	maxReceives int
	cancel      context.CancelFunc
}

func (f *fakeSQS) ReceiveMessage(_ context.Context, _ *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.receives++
	if f.receives > f.maxReceives {
		f.cancel()
		return &sqs.ReceiveMessageOutput{}, nil
	}
	return &sqs.ReceiveMessageOutput{Messages: f.msgs}, nil
}

func (f *fakeSQS) DeleteMessage(_ context.Context, in *sqs.DeleteMessageInput, _ ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, aws.ToString(in.ReceiptHandle))
	return &sqs.DeleteMessageOutput{}, nil
}

func TestConsumer_Run_DeletesOnlyHandledMessages(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fakeSQS{maxReceives: 1, cancel: cancel}
	f.msgs = []sqstypes.Message{
		{MessageId: aws.String("good"), ReceiptHandle: aws.String("rh-good"), Body: aws.String(body(t, msgAt(TypeLocked, "r1", 0)))},
		{MessageId: aws.String("bad"), ReceiptHandle: aws.String("rh-bad"), Body: aws.String("{garbage")},
	}
	store := NewStore(newFakeDynamo(), "test", 0)
	c := NewConsumer(f, "queue", "wallet", store, (&purgeRecorder{}).fn, newAckServer(t).client())
	if err := c.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.deleted) != 1 || f.deleted[0] != "rh-good" {
		t.Fatalf("deleted = %v, want only rh-good (poison stays for the DLQ)", f.deleted)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./erasure/ -run Consumer -count=1`
Expected: build FAIL: `undefined: NewConsumer`, `c.handle undefined`.

- [ ] **Step 4: Implement**

Create `erasure/consumer.go`:

```go
package erasure

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// PurgeFunc erases/anonymizes the service's data for m.Sub (and every org in
// m.Organizations) per the data-inventory spec. It must be idempotent and
// resumable, re-check eligibility first, and return Result done or blocked.
type PurgeFunc func(ctx context.Context, m Message) (Ack, error)

type sqsAPI interface {
	ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, opts ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, in *sqs.DeleteMessageInput, opts ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
}

type Consumer struct {
	sqs      sqsAPI
	queueURL string
	service  string
	store    *Store
	purge    PurgeFunc
	acks     *AckClient
	now      func() time.Time
}

func NewConsumer(sqsClient sqsAPI, queueURL, service string, store *Store, purge PurgeFunc, acks *AckClient) *Consumer {
	return &Consumer{sqs: sqsClient, queueURL: queueURL, service: service, store: store, purge: purge, acks: acks, now: time.Now}
}

// Run long-polls until ctx is cancelled. A message is deleted only after it was
// fully handled; anything else is redelivered and, past the queue's
// maxReceiveCount, lands in the DLQ.
// ponytail: one message at a time, no visibility heartbeat. Set the queue's
// visibility timeout to at least 2x the slowest purge; add a heartbeat if a
// purge ever outgrows it.
func (c *Consumer) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		out, err := c.sqs.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(c.queueURL),
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     20,
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.ErrorContext(ctx, "erasure: receive failed", "service", c.service, "error", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(5 * time.Second):
			}
			continue
		}
		for _, msg := range out.Messages {
			if err := c.handle(ctx, aws.ToString(msg.Body)); err != nil {
				slog.ErrorContext(ctx, "erasure: message left for redelivery",
					"service", c.service, "message_id", aws.ToString(msg.MessageId), "error", err)
				continue
			}
			if _, err := c.sqs.DeleteMessage(ctx, &sqs.DeleteMessageInput{
				QueueUrl:      aws.String(c.queueURL),
				ReceiptHandle: msg.ReceiptHandle,
			}); err != nil {
				slog.ErrorContext(ctx, "erasure: delete failed, message will be redelivered",
					"service", c.service, "message_id", aws.ToString(msg.MessageId), "error", err)
			}
		}
	}
	return nil
}

func (c *Consumer) handle(ctx context.Context, body string) error {
	m, err := Decode([]byte(body))
	if err != nil {
		return err
	}
	if !m.Targets(c.service) {
		return nil
	}
	key := SubKey(m.Sub)
	if m.Type != TypeErase {
		_, err := c.store.Apply(ctx, key, m)
		return err
	}

	// Lock first: the user.locked may have been lost, and writes must stop
	// before the purge starts. A no-op if already locked or erased.
	lock := m
	lock.Type = TypeLocked
	if _, err := c.store.Apply(ctx, key, lock); err != nil {
		return err
	}
	ack, err := c.purge(ctx, m)
	if err != nil {
		return fmt.Errorf("erasure: purge %s: %w", m.RequestID, err)
	}
	switch ack.Result {
	case ResultDone:
		for _, org := range m.Organizations {
			if _, err := c.store.Apply(ctx, OrgKey(org), m); err != nil {
				return err
			}
		}
		if _, err := c.store.Apply(ctx, key, m); err != nil {
			return err
		}
	case ResultBlocked:
		// Stays locked; ctech-account's support flow resolves and redrives.
	default:
		return fmt.Errorf("erasure: purge %s returned result %q", m.RequestID, ack.Result)
	}
	ack.RequestID, ack.Service, ack.At = m.RequestID, c.service, c.now().UTC()
	return c.acks.Send(ctx, ack)
}
```

- [ ] **Step 5: Run the whole suite**

Run: `go vet ./... && go build ./... && go test ./... -race -count=1`
Expected: PASS (all packages).

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum erasure/consumer.go erasure/consumer_test.go
git commit -m "feat(erasure): SQS consumer that acks only fully handled erasures"
```

---

### Task 6: Documentation and release

**Files:**
- Modify: `README.md` (package table, new "Account erasure" section, `jwtverify` revocation note)
- Modify: `AGENTS.md` ("What lives here" index: `jwtverify` entry, new `erasure` entry)

**Interfaces:**
- Consumes: every symbol from Tasks 1–5 (documentation only).

- [ ] **Step 1: README package table**

Add this row to the package table in `README.md`, after the `ratelimit` row:

```markdown
| `erasure`      | Participant side of the LGPD account-deletion saga: message contract (`Message`, `Encode`/`Decode`), `{prefix}_erasure_state` lock/tombstone `Store`, SQS `Consumer`, `AckClient`, eligibility/blocker types. Orchestration lives in ctech-account |
```

- [ ] **Step 2: README section**

Add after the "Error observability" section:

```markdown
## Account erasure (LGPD deletion)

Contract: `ctech-account/docs/specs/2026-10-06-account-deletion-saga-protocol.md`.

Each participant service (wallet, dfe, billing, poker):

1. Creates a `{prefix}_erasure_state` DynamoDB table (partition key `pk` String, TTL attribute `ttl`)
   and an SQS queue + DLQ subscribed to ctech-account's `{env}-account-user-erasure` SNS topic
   (filter policy on `services`, scope `MessageBody`; raw delivery recommended, the envelope is also accepted).
   Queue visibility timeout ≥ 2× the slowest purge.
2. Builds `erasure.NewStore(dynamoClient, tablePrefix, retention)` and calls `store.Blocked(ctx, sub)` on
   every user write path (refuse when true), and `store.OrgErased` on org-scoped async work.
3. Implements a `PurgeFunc` (idempotent, resumable, re-checks eligibility, returns `done`/`blocked`) and
   runs `erasure.NewConsumer(sqsClient, queueURL, "<service>", store, purge, erasure.NewAckClient(...)).Run(ctx)`
   in a background goroutine. The ack client uses an `oauth2client.TokenManager` with scope `account:erasure:ack`.
4. Serves `GET /internal/erasure/eligibility/{sub}` returning `erasure.NewEligibility(blockers...)`.

Messages arrive out of order: lock/unlock are ordered by `issued_at`, and `erased` is terminal.

### JWT revocation

`Verifier.WithRevocation(backend)` rejects tokens of a locked user (`ErrTokenRevoked`). ctech-account writes the
entries with `jwtverify.Revoke` / `jwtverify.Unrevoke`. The entries live in **Valkey DB 0** (the shared base URL
`/ctech/{env}/valkey/url`, no DB suffix): services whose main cache uses another logical DB (wallet DB 2,
billing DB 3) must pass a second `cache.RedisBackend` built on the base URL. `VerifyClaims` fails open if Valkey is
unreachable; money-moving routes use `VerifyClaimsStrict`, which fails closed with `ErrRevocationUnavailable`.
```

- [ ] **Step 3: AGENTS.md index**

In `AGENTS.md` "What lives here", append to the `jwtverify` entry:

```markdown
  Revocation: `WithRevocation`, `Revoke`, `Unrevoke`, `RevocationTTL`, `ErrTokenRevoked`, `ErrRevocationUnavailable`,
  `VerifyClaimsStrict` in `jwtverify/revocation.go` / `jwtverify/verifier.go` (entries in Valkey DB 0).
```

and add a new entry after it:

```markdown
- `erasure` — LGPD account-deletion participant contract. `Message`/`Encode`/`Decode` `erasure/message.go`;
  `Store` (`Apply`, `Blocked`, `OrgErased`, `Clear`) over `{prefix}_erasure_state` `erasure/state.go`
  (pure transition `apply`: issued_at ordering, erased terminal); `Consumer` `erasure/consumer.go`
  (deletes a message only after purge + tombstone + ack); `AckClient` `erasure/ack.go`.
```

- [ ] **Step 4: Verify and commit**

Run: `go vet ./... && go test ./... -race -count=1`
Expected: PASS

```bash
git add README.md AGENTS.md
git commit -m "docs: erasure package and jwtverify revocation"
```

- [ ] **Step 5: Release (ask the user first)**

Pushing and tagging publishes the module to every consumer. **Ask for confirmation**, then:

```bash
git push origin main
git tag v1.13.0
git push origin v1.13.0
```

---

## Follow-up plans (not in this plan)

Each one is a separate plan in its own repo, in this order (overview §9):

1. `ctech-account`: deletion domain, endpoints, orchestrator, SNS topic + `deletion_requests` table, own purge, UI.
2. `ctech-poker`, then `ctech-billing`, then `ctech-dfe`, then `ctech-wallet`: `erasure_state` table, queue + DLQ, `PurgeFunc`, eligibility endpoint, `Blocked` guards, `WithRevocation` (+ `VerifyClaimsStrict` on wallet money routes), bump to `v1.13.0`.
