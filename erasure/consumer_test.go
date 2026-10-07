package erasure

import (
	"context"
	"errors"
	"net/http"
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
