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
