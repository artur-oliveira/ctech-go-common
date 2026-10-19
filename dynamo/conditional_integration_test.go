//go:build integration

// Integration tests for UpdateVersioned and the cursor envelopes against
// DynamoDB Local (see query_integration_test.go for how to start it).
package dynamo

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Concurrent writers each add 1 to a counter. With the version guard every
// increment lands exactly once or fails loudly: none is silently lost.
func TestUpdateVersionedNoLostUpdates(t *testing.T) {
	b, _ := newQueryTable(t)
	ctx := context.Background()
	if err := b.PutItem(ctx, map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: "ctr"}, "sk": &types.AttributeValueMemberS{Value: "a"},
		"n": &types.AttributeValueMemberN{Value: "0"},
	}); err != nil {
		t.Fatal(err)
	}
	sk := "a"
	const writers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		applied int
	)
	for range writers {
		wg.Go(func() {
			err := b.UpdateVersioned(ctx, "ctr", &sk, func(it map[string]types.AttributeValue) (map[string]any, error) {
				var n int
				_ = attributevalue.Unmarshal(it["n"], &n)
				return map[string]any{"n": n + 1}, nil
			}, writers)
			if err != nil && !errors.Is(err, ErrVersionConflict) {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if err == nil {
				mu.Lock()
				applied++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	item, err := b.GetItem(ctx, "ctr", sk)
	if err != nil {
		t.Fatal(err)
	}
	var n, version int
	_ = attributevalue.Unmarshal(item["n"], &n)
	_ = attributevalue.Unmarshal(item["version"], &version)
	if n != applied || version != applied || applied == 0 {
		t.Fatalf("n=%d version=%d applied=%d: an applied update was lost", n, version, applied)
	}
}

func TestUpdateVersionedMissingItem(t *testing.T) {
	b, _ := newQueryTable(t)
	sk := "a"
	err := b.UpdateVersioned(context.Background(), "nope", &sk, func(map[string]types.AttributeValue) (map[string]any, error) {
		return map[string]any{"n": 1}, nil
	}, 0)
	if !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestConditionalUpdateAgainstDynamo(t *testing.T) {
	b, _ := newQueryTable(t)
	ctx := context.Background()
	sk := "a"
	_ = b.PutItem(ctx, map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: "c"}, "sk": &types.AttributeValueMemberS{Value: sk},
		"status": &types.AttributeValueMemberS{Value: "pending"},
	})
	cond := func(want string) (bool, error) {
		return b.ConditionalUpdate(ctx, "c", &sk, map[string]any{"status": "done"}, "#status = :want",
			map[string]string{"#status": "status"},
			map[string]types.AttributeValue{":want": &types.AttributeValueMemberS{Value: want}})
	}
	if ok, err := cond("other"); ok || err != nil {
		t.Fatalf("mismatch: ok=%v err=%v", ok, err)
	}
	if ok, err := cond("pending"); !ok || err != nil {
		t.Fatalf("match: ok=%v err=%v", ok, err)
	}
	if ok, err := cond("pending"); ok || err != nil {
		t.Fatalf("single use: ok=%v err=%v", ok, err)
	}
}

// Pages through a real partition with the cursor envelope, forward and back.
func TestCursorPagingAgainstDynamo(t *testing.T) {
	const owner = "org-1"
	b, _ := newQueryTable(t)
	ctx := context.Background()
	want := seed(t, b, "P", 7, func(int) bool { return true })

	var got []string
	var incoming string
	var cursors []string
	for range 10 {
		start, err := DecodeCursor(incoming, owner)
		if err != nil {
			t.Fatal(err)
		}
		res, err := b.Query(ctx, QueryOpts{PK: "P", ScanIndexForward: true, Limit: 3, ExclusiveStartKey: start})
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, sks(t, res.Items)...)
		page := NewPageWithPrevious(sks(t, res.Items), res.LastEvaluatedKey, incoming, owner)
		cursors = append(cursors, incoming)
		if page.NextCursor == nil {
			break
		}
		incoming = *page.NextCursor
		if len(cursors) == 3 { // page 3's previous_cursor must reopen page 2
			p3 := NewPageWithPrevious([]string{}, res.LastEvaluatedKey, cursors[2], owner)
			if p3.PreviousCursor == nil {
				t.Fatal("page 3 must offer previous_cursor")
			}
			start, err := DecodeCursor(*p3.PreviousCursor, owner)
			if err != nil {
				t.Fatal(err)
			}
			back, err := b.Query(ctx, QueryOpts{PK: "P", ScanIndexForward: true, Limit: 3, ExclusiveStartKey: start})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(sks(t, back.Items), want[3:6]) {
				t.Fatalf("previous page = %v, want %v", sks(t, back.Items), want[3:6])
			}
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("paged %v, want %v", got, want)
	}
}
