//go:build integration

package pushsvc

import (
	"strconv"

	"context"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"sync"
	"testing"
	"time"
)

func sub(n int) Subscription {
	return Subscription{Endpoint: fmt.Sprintf("https://fcm.googleapis.com/fcm/send/%d", n), P256dh: "p", Auth: "a", VapidKID: "k", Device: "Chrome"}
}

func newStore(t *testing.T) *Store {
	t.Helper()
	db, prefix := newTestDB(t)
	return NewStore(db, prefix)
}

func TestUpsertEvictsOldestAt11(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	var ids []string
	for i := 0; i < MaxSubscriptionsPerUser; i++ {
		id, err := st.Upsert(ctx, "u1", sub(i))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		time.Sleep(2 * time.Millisecond) // last_seen_at orders the eviction
	}
	if _, err := st.Upsert(ctx, "u1", sub(99)); err != nil {
		t.Fatal(err)
	}
	list, err := st.List(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != MaxSubscriptionsPerUser {
		t.Fatalf("len = %d, want %d", len(list), MaxSubscriptionsPerUser)
	}
	for _, d := range list {
		if d.ID == ids[0] {
			t.Error("the least recently seen device survived")
		}
	}
}

func TestUpsertRenewKeepsCreatedAt(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	id, _ := st.Upsert(ctx, "u1", sub(1))
	first, _ := st.List(ctx, "u1")
	time.Sleep(5 * time.Millisecond)
	if id2, err := st.Upsert(ctx, "u1", sub(1)); err != nil || id2 != id {
		t.Fatalf("renew = %q, %v", id2, err)
	}
	second, _ := st.List(ctx, "u1")
	if len(second) != 1 || second[0].CreatedAt != first[0].CreatedAt || second[0].LastSeenAt <= first[0].LastSeenAt {
		t.Errorf("first %+v second %+v", first, second)
	}
}

func TestDeleteFreesSlot(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	id, _ := st.Upsert(ctx, "u1", sub(1))
	if err := st.Delete(ctx, "u1", id); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, "u1", id); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete = %v, want ErrNotFound", err)
	}
	if list, _ := st.List(ctx, "u1"); len(list) != 0 {
		t.Errorf("list = %v", list)
	}
}

func TestOtherOwnersFindsPreviousUser(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	_, _ = st.Upsert(ctx, "alice", sub(1))
	// The endpoint-index is a GSI: eventually consistent. DynamoDB Local applies it immediately.
	owners, err := st.OtherOwners(ctx, sub(1).Endpoint, "bob")
	if err != nil || len(owners) != 1 || owners[0] != "alice" {
		t.Fatalf("owners = %v, %v", owners, err)
	}
	if owners, _ := st.OtherOwners(ctx, sub(1).Endpoint, "alice"); len(owners) != 0 {
		t.Errorf("own row listed as other owner: %v", owners)
	}
}

func TestConcurrentUpsertNeverExceeds10(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = st.Upsert(ctx, "u1", sub(i)) // a lost race may error; the invariant is what is checked
		}()
	}
	wg.Wait()
	list, err := st.List(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) > MaxSubscriptionsPerUser {
		t.Errorf("rows = %d, over the limit", len(list))
	}
	n, err := st.count(ctx, "u1")
	if err != nil || n > MaxSubscriptionsPerUser {
		t.Errorf("count row = %d, %v", n, err)
	}
}

// count reads the guard row.
func (s *Store) count(ctx context.Context, sub string) (int, error) {
	it, err := s.base.GetItem(ctx, userPK(sub), countsSK)
	if err != nil || it == nil {
		return 0, err
	}
	n, ok := it["subscriptions"].(*types.AttributeValueMemberN)
	if !ok {
		return 0, nil
	}
	return strconv.Atoi(n.Value)
}
