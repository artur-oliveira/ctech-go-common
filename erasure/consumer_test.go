package erasure

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestConsumer_StaleEraseAfterClear_DoesNotPurge(t *testing.T) {
	ctx := context.Background()
	p := &purgeRecorder{ack: Ack{Result: ResultDone}}
	c, store, a := newTestConsumer(t, "wallet", p)
	store.now = func() time.Time { return t0.Add(time.Hour) }
	b := body(t, msgAt(TypeErase, "r1", 0))
	if err := c.handle(ctx, b); err != nil {
		t.Fatalf("erase: %v", err)
	}
	if err := store.Clear(ctx, "user-1"); err != nil { // user re-consented
		t.Fatalf("Clear: %v", err)
	}
	if err := c.handle(ctx, b); err != nil { // late duplicate
		t.Fatalf("late erase: %v", err)
	}
	if p.calls != 1 {
		t.Fatalf("purge calls = %d, want 1: a stale erase must not wipe the returning user's data", p.calls)
	}
	if blocked, _ := store.Blocked(ctx, "user-1"); blocked {
		t.Fatal("returning user must not be locked by a stale erase")
	}
	if acks := a.received(); len(acks) != 2 || acks[1].Result != ResultDone {
		t.Fatalf("stale erase must still be acked done (work was done before): %+v", acks)
	}
}

type countingSQS struct {
	fakeSQS
	maxPerReceive []int32
}

func (f *countingSQS) ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, opts ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	f.maxPerReceive = append(f.maxPerReceive, in.MaxNumberOfMessages)
	return f.fakeSQS.ReceiveMessage(ctx, in, opts...)
}

func TestConsumer_Run_ReceivesOneMessageAtATime(t *testing.T) {
	// Every received message's visibility clock starts at receive; with a batch,
	// later messages reappear on other replicas while earlier ones still purge.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &countingSQS{fakeSQS: fakeSQS{maxReceives: 1, cancel: cancel}}
	c := NewConsumer(f, "queue", "wallet", NewStore(newFakeDynamo(), "test", 0), (&purgeRecorder{}).fn, newAckServer(t).client())
	_ = c.Run(ctx)
	for _, n := range f.maxPerReceive {
		if n != 1 {
			t.Fatalf("MaxNumberOfMessages = %d, want 1", n)
		}
	}
}

func TestConsumer_StaleEraseFromOtherRequest_NotAckedDone(t *testing.T) {
	// The record is active because of a different, newer request: nothing proves
	// this erase was ever purged, so it must not be reported done.
	ctx := context.Background()
	p := &purgeRecorder{ack: Ack{Result: ResultDone}}
	c, store, a := newTestConsumer(t, "wallet", p)
	_, _ = store.Apply(ctx, SubKey("user-1"), msgAt(TypeUnlocked, "r2", time.Hour))
	if err := c.handle(ctx, body(t, msgAt(TypeErase, "r1", 0))); err == nil {
		t.Fatal("unprovable stale erase must be left for redelivery/DLQ, not acked")
	}
	if p.calls != 0 || len(a.received()) != 0 {
		t.Fatalf("purge calls=%d acks=%d, want 0/0", p.calls, len(a.received()))
	}
}

func TestConsumer_OnLockFiresOnceWhenTheStateMoves(t *testing.T) {
	ctx := context.Background()
	c, _, _ := newTestConsumer(t, "wallet", &purgeRecorder{ack: Ack{Result: ResultDone}})
	var got []string
	c.OnLock(func(_ context.Context, sub string) { got = append(got, sub) })

	lock := body(t, msgAt(TypeLocked, "r1", 0))
	for i := 0; i < 2; i++ { // the second delivery is a no-op
		if err := c.handle(ctx, lock); err != nil {
			t.Fatalf("lock: %v", err)
		}
	}
	if len(got) != 1 || got[0] != "user-1" {
		t.Fatalf("OnLock calls = %v, want exactly one for user-1", got)
	}
	// An unlock is not a lock.
	if err := c.handle(ctx, body(t, msgAt(TypeUnlocked, "r1", 1))); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("OnLock fired on unlock: %v", got)
	}
	// A new request locks again.
	if err := c.handle(ctx, body(t, msgAt(TypeLocked, "r2", 2))); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("OnLock calls = %v, want a second one for the new lock", got)
	}
}

// The synthetic lock before an erase fires the hook when the user.locked was
// lost; an erase after a delivered lock does not fire it again.
func TestConsumer_OnLockFiresForTheSyntheticLockOfAnErase(t *testing.T) {
	ctx := context.Background()
	c, _, _ := newTestConsumer(t, "wallet", &purgeRecorder{ack: Ack{Result: ResultDone}})
	calls := 0
	c.OnLock(func(context.Context, string) { calls++ })
	if err := c.handle(ctx, body(t, msgAt(TypeErase, "r1", 0))); err != nil {
		t.Fatalf("erase: %v", err)
	}
	if calls != 1 {
		t.Fatalf("OnLock calls = %d, want 1 for the synthetic lock", calls)
	}

	c2, _, _ := newTestConsumer(t, "wallet", &purgeRecorder{ack: Ack{Result: ResultDone}})
	calls = 0
	c2.OnLock(func(context.Context, string) { calls++ })
	_ = c2.handle(ctx, body(t, msgAt(TypeLocked, "r1", 0)))
	if err := c2.handle(ctx, body(t, msgAt(TypeErase, "r1", 1))); err != nil {
		t.Fatalf("erase after lock: %v", err)
	}
	if calls != 1 {
		t.Fatalf("OnLock calls = %d, want 1 (the erase must not fire it again)", calls)
	}
}

func TestConsumer_OnLockPanicIsContained(t *testing.T) {
	ctx := context.Background()
	c, store, _ := newTestConsumer(t, "wallet", &purgeRecorder{})
	c.OnLock(func(context.Context, string) { panic("boom") })
	if err := c.handle(ctx, body(t, msgAt(TypeLocked, "r1", 0))); err != nil {
		t.Fatalf("a panicking hook must not fail the message: %v", err)
	}
	if blocked, _ := store.Blocked(ctx, "user-1"); !blocked {
		t.Fatal("the lock must be persisted regardless of the hook")
	}
}

func relinkMsg(offset time.Duration) Message {
	m := msgAt(TypeRelinked, "relink-1", offset)
	m.Scope, m.Services = ScopeService, []string{"wallet"}
	return m
}

func TestConsumer_RelinkedClearsAnErasedSub(t *testing.T) {
	ctx := context.Background()
	p := &purgeRecorder{ack: Ack{Result: ResultDone}}
	c, store, a := newTestConsumer(t, "wallet", p)
	store.now = func() time.Time { return t0.Add(time.Hour) }
	erase := msgAt(TypeErase, "r1", 0)
	erase.Scope = ScopeService
	if err := c.handle(ctx, body(t, erase)); err != nil {
		t.Fatalf("erase: %v", err)
	}
	if err := c.handle(ctx, body(t, relinkMsg(time.Minute))); err != nil {
		t.Fatalf("relink: %v", err)
	}
	if blocked, _ := store.Blocked(ctx, "user-1"); blocked {
		t.Fatal("a person who came back must not stay blocked")
	}
	if len(a.received()) != 1 {
		t.Fatalf("acks = %+v; a relink owes no ack", a.received())
	}
}

func TestConsumer_RelinkedOnAnActiveSubIsANoOp(t *testing.T) {
	ctx := context.Background()
	c, store, a := newTestConsumer(t, "wallet", &purgeRecorder{})
	if err := c.handle(ctx, body(t, relinkMsg(0))); err != nil {
		t.Fatalf("relink: %v", err)
	}
	if r, _ := store.Get(ctx, SubKey("user-1")); r != nil {
		t.Fatalf("record = %+v, want none", r)
	}
	if len(a.received()) != 0 {
		t.Fatal("no ack expected")
	}
}

func TestConsumer_RelinkedForAnotherServiceIsIgnored(t *testing.T) {
	ctx := context.Background()
	c, store, _ := newTestConsumer(t, "dfe", &purgeRecorder{ack: Ack{Result: ResultDone}})
	store.now = func() time.Time { return t0.Add(time.Hour) }
	erase := msgAt(TypeErase, "r1", 0)
	if err := c.handle(ctx, body(t, erase)); err != nil {
		t.Fatalf("erase: %v", err)
	}
	if err := c.handle(ctx, body(t, relinkMsg(time.Minute))); err != nil { // addressed to wallet
		t.Fatalf("relink: %v", err)
	}
	if blocked, _ := store.Blocked(ctx, "user-1"); !blocked {
		t.Fatal("dfe's tombstone must survive a relink addressed to wallet")
	}
}

func TestConsumer_StaleEraseAfterRelinkIsAckedNotPurged(t *testing.T) {
	ctx := context.Background()
	p := &purgeRecorder{ack: Ack{Result: ResultDone}}
	c, store, a := newTestConsumer(t, "wallet", p)
	store.now = func() time.Time { return t0.Add(time.Hour) }
	erase := msgAt(TypeErase, "r1", 0)
	erase.Scope = ScopeService
	b := body(t, erase)
	if err := c.handle(ctx, b); err != nil {
		t.Fatalf("erase: %v", err)
	}
	if err := c.handle(ctx, body(t, relinkMsg(time.Minute))); err != nil {
		t.Fatalf("relink: %v", err)
	}
	if err := c.handle(ctx, b); err != nil { // late duplicate of the old erase
		t.Fatalf("late erase: %v", err)
	}
	if p.calls != 1 {
		t.Fatalf("purge calls = %d, want 1: the returning person's new data must survive", p.calls)
	}
	if acks := a.received(); len(acks) != 2 || acks[1].Result != ResultDone {
		t.Fatalf("acks = %+v, want the stale erase acked done", acks)
	}
}

// A relink delivered late, after the person left again, must not reopen the
// newer tombstone: relink is ordered by issued_at like lock/unlock.
func TestConsumer_StaleRelinkAfterANewerEraseIsIgnored(t *testing.T) {
	ctx := context.Background()
	p := &purgeRecorder{ack: Ack{Result: ResultDone}}
	c, store, _ := newTestConsumer(t, "wallet", p)
	first := msgAt(TypeErase, "r1", 0)
	first.Scope = ScopeService
	old := body(t, relinkMsg(time.Hour))
	for _, b := range []string{body(t, first), old} {
		if err := c.handle(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	second := msgAt(TypeErase, "r2", 2*time.Hour) // the person left again
	second.Scope = ScopeService
	if err := c.handle(ctx, body(t, second)); err != nil {
		t.Fatalf("second erase: %v", err)
	}
	if err := c.handle(ctx, old); err != nil { // duplicate of the first relink
		t.Fatalf("late relink: %v", err)
	}
	if blocked, _ := store.Blocked(ctx, "user-1"); !blocked {
		t.Fatal("a stale relink reopened a newer erasure")
	}
}

func TestConsumerRoutesClaimedBodiesToTheExporter(t *testing.T) {
	c := &Consumer{service: "poker"}
	var seen string
	c.WithExporter(func(_ context.Context, body []byte) (bool, error) {
		if strings.Contains(string(body), "user.export") {
			seen = string(body)
			return true, nil
		}
		return false, nil
	})
	// An export body is claimed: no erasure decoding (it would fail validation) and no store access.
	if err := c.handle(context.Background(), `{"type":"user.export"}`); err != nil || seen == "" {
		t.Fatalf("err=%v seen=%q", err, seen)
	}
	// Anything else still goes down the erasure path, which rejects garbage.
	if err := c.handle(context.Background(), `{"type":"nope"}`); err == nil {
		t.Fatal("non-export body must still be decoded as erasure")
	}
}
