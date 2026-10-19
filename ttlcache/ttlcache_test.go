package ttlcache

import (
	"testing"
	"time"
)

func TestEntriesExpireAndCanBeDeleted(t *testing.T) {
	now := time.Unix(0, 0)
	c := New[int](10*time.Second, 8)
	c.now = func() time.Time { return now }

	c.Set("a", 1)
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatalf("fresh entry: %v %v", v, ok)
	}
	now = now.Add(10 * time.Second)
	if _, ok := c.Get("a"); ok {
		t.Fatal("entry survived its TTL")
	}
	c.Set("b", 2)
	c.Delete("b")
	if _, ok := c.Get("b"); ok {
		t.Fatal("deleted entry still served")
	}
}

func TestAtCapacityItDropsExpiredThenEverything(t *testing.T) {
	now := time.Unix(0, 0)
	c := New[int](10*time.Second, 2)
	c.now = func() time.Time { return now }

	c.Set("old", 1)
	now = now.Add(11 * time.Second)
	c.Set("x", 2)
	c.Set("y", 3) // full: "old" is expired and goes, "x" stays
	if _, ok := c.Get("x"); !ok {
		t.Fatal("a live entry was dropped while an expired one could go")
	}
	c.Set("z", 4) // full of live entries: flush, then store
	if _, ok := c.Get("z"); !ok {
		t.Fatal("the new entry was not stored")
	}
	if len(c.m) > 2 {
		t.Fatalf("%d entries, max 2", len(c.m))
	}
}
