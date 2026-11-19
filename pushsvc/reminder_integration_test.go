//go:build integration

package pushsvc

import (
	"context"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"testing"
)

func newReminders(t *testing.T) *ReminderStore {
	t.Helper()
	db, prefix := newTestDB(t)
	return NewReminderStore(db, prefix)
}

func TestMarkSentIsOncePerDay(t *testing.T) {
	ctx := context.Background()
	rs := newReminders(t)
	if err := rs.Put(ctx, Reminder{Sub: "u1", Kind: "streak", Enabled: true, Slot: 80}); err != nil {
		t.Fatal(err)
	}
	for i, want := range []bool{true, false} {
		if got, err := rs.MarkSent(ctx, "u1", "streak", "2026-11-18"); err != nil || got != want {
			t.Fatalf("call %d = %v, %v; want %v", i, got, err, want)
		}
	}
	if got, err := rs.MarkSent(ctx, "u1", "streak", "2026-11-19"); err != nil || !got {
		t.Errorf("next day = %v, %v", got, err)
	}
}

func TestMarkSentOnMissingRowIsFalse(t *testing.T) {
	if got, err := newReminders(t).MarkSent(context.Background(), "ghost", "streak", "2026-11-18"); err != nil || got {
		t.Errorf("got %v, %v; a missing reminder must not be created by a mark", got, err)
	}
}

func TestPutDisabledLeavesSlotIndex(t *testing.T) {
	ctx := context.Background()
	rs := newReminders(t)
	_ = rs.Put(ctx, Reminder{Sub: "u1", Kind: "play", Enabled: true, Slot: 76})
	page, _, err := rs.ListSlot(ctx, 76, 10, nil)
	if err != nil || len(page) != 1 || page[0].Sub != "u1" || page[0].Kind != "play" {
		t.Fatalf("page = %+v, %v", page, err)
	}
	_ = rs.Put(ctx, Reminder{Sub: "u1", Kind: "play", Enabled: false, Slot: 76})
	if page, _, _ := rs.ListSlot(ctx, 76, 10, nil); len(page) != 0 {
		t.Errorf("a disabled reminder is still in the slot: %+v", page)
	}
	if got, ok, _ := rs.Get(ctx, "u1", "play"); !ok || got.Enabled || got.Slot != 76 {
		t.Errorf("Get = %+v, %v", got, ok)
	}
}

func TestPutPreservesLastSentDay(t *testing.T) {
	ctx := context.Background()
	rs := newReminders(t)
	_ = rs.Put(ctx, Reminder{Sub: "u1", Kind: "streak", Enabled: true, Slot: 80})
	_, _ = rs.MarkSent(ctx, "u1", "streak", "2026-11-18")
	_ = rs.Put(ctx, Reminder{Sub: "u1", Kind: "streak", Enabled: true, Slot: 84}) // the user moves the time
	got, _, _ := rs.Get(ctx, "u1", "streak")
	if got.LastSentDay != "2026-11-18" || got.Slot != 84 {
		t.Errorf("got %+v; changing the time must not allow a second send the same day", got)
	}
}

func TestListSlotPaginates(t *testing.T) {
	ctx := context.Background()
	rs := newReminders(t)
	for _, u := range []string{"a", "b", "c", "d", "e"} {
		_ = rs.Put(ctx, Reminder{Sub: u, Kind: "streak", Enabled: true, Slot: 10})
	}
	_ = rs.Put(ctx, Reminder{Sub: "z", Kind: "streak", Enabled: true, Slot: 11})
	seen := map[string]bool{}
	var cursor map[string]types.AttributeValue
	for {
		page, next, err := rs.ListSlot(ctx, 10, 2, cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range page {
			seen[r.Sub] = true
		}
		if next == nil {
			break
		}
		cursor = next
	}
	if len(seen) != 5 || seen["z"] {
		t.Errorf("seen = %v", seen)
	}
}

func TestDeleteAllRemovesOnlyThatUsersReminders(t *testing.T) {
	ctx := context.Background()
	rs := newReminders(t)
	_ = rs.Put(ctx, Reminder{Sub: "u1", Kind: "streak", Enabled: true, Slot: 1})
	_ = rs.Put(ctx, Reminder{Sub: "u1", Kind: "play", Enabled: true, Slot: 2})
	_ = rs.Put(ctx, Reminder{Sub: "u2", Kind: "play", Enabled: true, Slot: 2})
	if err := rs.DeleteAll(ctx, "u1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := rs.Get(ctx, "u1", "streak"); ok {
		t.Error("u1 streak survived")
	}
	if _, ok, _ := rs.Get(ctx, "u2", "play"); !ok {
		t.Error("u2 was purged")
	}
}

func TestMoveSlotOnlyWhileEnabledAndUnmoved(t *testing.T) {
	ctx := context.Background()
	rs := newReminders(t)
	_ = rs.Put(ctx, Reminder{Sub: "u1", Kind: "play", Enabled: true, Slot: 76})
	if ok, err := rs.MoveSlot(ctx, "u1", "play", 76, 81); err != nil || !ok {
		t.Fatalf("move = %v, %v", ok, err)
	}
	if page, _, _ := rs.ListSlot(ctx, 81, 10, nil); len(page) != 1 {
		t.Errorf("the reminder is not in its new slot: %+v", page)
	}
	if page, _, _ := rs.ListSlot(ctx, 76, 10, nil); len(page) != 0 {
		t.Errorf("the reminder is still in its old slot: %+v", page)
	}
	// The player moved it themselves meanwhile: a stale move must lose.
	if ok, _ := rs.MoveSlot(ctx, "u1", "play", 76, 90); ok {
		t.Error("a move from a slot the reminder no longer has was applied")
	}
	// The player switched it off meanwhile: a move must never switch it back on.
	_ = rs.Put(ctx, Reminder{Sub: "u1", Kind: "play", Enabled: false, Slot: 81})
	if ok, _ := rs.MoveSlot(ctx, "u1", "play", 81, 82); ok {
		t.Error("a disabled reminder was moved")
	}
	if got, _, _ := rs.Get(ctx, "u1", "play"); got.Enabled {
		t.Errorf("got %+v: the move resurrected an opt-out", got)
	}
	if page, _, _ := rs.ListSlot(ctx, 82, 10, nil); len(page) != 0 {
		t.Errorf("a disabled reminder reappeared in the index: %+v", page)
	}
}
