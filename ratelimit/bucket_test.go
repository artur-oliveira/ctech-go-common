package ratelimit

import (
	"fmt"
	"testing"
	"time"
)

func clocked(b *Bucket) *time.Time {
	t := time.Unix(1000, 0)
	b.now = func() time.Time { return t }
	return &t
}

func TestBucketAllowsABurstThenRefusesThenRefills(t *testing.T) {
	b := NewBucket(2, 3, 0)
	now := clocked(b)
	for i := 0; i < 3; i++ {
		if ok, _ := b.Allow("a"); !ok {
			t.Fatalf("request %d of the burst refused", i)
		}
	}
	ok, wait := b.Allow("a")
	if ok || wait != 500*time.Millisecond {
		t.Fatalf("a fourth must wait for half a second at 2/s: ok=%v wait=%v", ok, wait)
	}
	*now = now.Add(500 * time.Millisecond)
	if ok, _ := b.Allow("a"); !ok {
		t.Fatal("a token has refilled")
	}
	if ok, _ := b.Allow("a"); ok {
		t.Fatal("only one")
	}
}

func TestBucketKeysAreIndependentAndNilAllowsAll(t *testing.T) {
	b := NewBucket(1, 1, 0)
	clocked(b)
	if ok, _ := b.Allow("a"); !ok {
		t.Fatal("a")
	}
	if ok, _ := b.Allow("a"); ok {
		t.Fatal("a is spent")
	}
	if ok, _ := b.Allow("b"); !ok {
		t.Fatal("b has its own bucket")
	}
	var off *Bucket
	for i := 0; i < 100; i++ {
		if ok, _ := off.Allow("x"); !ok {
			t.Fatal("a nil bucket is off")
		}
	}
	if NewBucket(0, 5, 0) != nil || NewBucket(5, 0, 0) != nil {
		t.Fatal("a zero rate or burst is off")
	}
}

func TestBucketMemoryIsBoundedAndNeverRefusesBecauseOfIt(t *testing.T) {
	b := NewBucket(1, 1, 3)
	now := clocked(b)
	for i := 0; i < 3; i++ {
		b.Allow(fmt.Sprint("k", i)) // three spent buckets fill the table
	}
	if ok, _ := b.Allow("new"); !ok {
		t.Fatal("a key that does not fit is allowed, not refused")
	}
	if len(b.entries) != 3 {
		t.Fatalf("the table grew to %d", len(b.entries))
	}
	*now = now.Add(5 * time.Second) // everything refilled: idle keys go
	b.Allow("fresh")
	if len(b.entries) > 3 || b.entries["fresh"] == nil {
		t.Fatalf("idle keys must make room: %d", len(b.entries))
	}
}
