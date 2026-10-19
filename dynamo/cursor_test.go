package dynamo

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"gopkg.aoctech.app/api-commons/problem"
)

func s(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }

func TestParseLimit(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
	}{
		{"", DefaultPageLimit}, {"abc", DefaultPageLimit}, {"0", DefaultPageLimit}, {"-5", DefaultPageLimit},
		{"1", 1}, {"100", 100}, {"101", MaxPageLimit}, {"999999999999", MaxPageLimit},
	} {
		if got := ParseLimit(tc.raw); got != tc.want {
			t.Errorf("ParseLimit(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}

func TestCursorRoundTripKeepsKeyTypes(t *testing.T) {
	key := map[string]types.AttributeValue{
		"pk":  s("ORG#1"),
		"sk":  &types.AttributeValueMemberN{Value: "9007199254740993"}, // > 2^53: a float64 round trip would corrupt it
		"bin": &types.AttributeValueMemberB{Value: []byte{0, 1, 2, 255}},
	}
	cur := EncodeCursor(key, "org-1")
	got, err := DecodeCursor(cur, "org-1")
	if err != nil {
		t.Fatal(err)
	}
	if n := got["sk"].(*types.AttributeValueMemberN).Value; n != "9007199254740993" {
		t.Errorf("number key = %s", n)
	}
	if string(got["bin"].(*types.AttributeValueMemberB).Value) != string([]byte{0, 1, 2, 255}) {
		t.Errorf("binary key mangled")
	}
	if got["pk"].(*types.AttributeValueMemberS).Value != "ORG#1" {
		t.Errorf("string key mangled")
	}
	if strings.ContainsAny(cur, "+/=") {
		t.Errorf("cursor %q is not URL-safe", cur)
	}
}

func TestEncodeCursorEmptyAndUnsupported(t *testing.T) {
	if EncodeCursor(nil, "o") != "" {
		t.Error("nil key must give no cursor")
	}
	if EncodeCursor(map[string]types.AttributeValue{"pk": &types.AttributeValueMemberBOOL{Value: true}}, "o") != "" {
		t.Error("non-key type must give no cursor")
	}
}

func TestDecodeCursor(t *testing.T) {
	good := EncodeCursor(map[string]types.AttributeValue{"pk": s("a")}, "org-1")
	enc := func(v string) string { return base64.RawURLEncoding.EncodeToString([]byte(v)) }
	for _, tc := range []struct {
		name, cursor, owner string
		wantNil, wantErr    bool
	}{
		{"empty is the start", "", "org-1", true, false},
		{"valid", good, "org-1", false, false},
		{"other owner", good, "org-2", false, true},
		{"owner missing", good, "", false, true},
		{"not base64", "!!!", "org-1", false, true},
		{"not json", enc("nope"), "org-1", false, true},
		{"no key", enc(`{"k":{}}`), "org-1", false, true},
		{"unknown type", enc(`{"k":{"pk":{"t":"X","v":"a"}}}`), "", false, true},
		{"bad binary", enc(`{"k":{"pk":{"t":"B","v":"***"}}}`), "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, err := DecodeCursor(tc.cursor, tc.owner)
			if tc.wantErr {
				p, ok := errors.AsType[*problem.Problem](err)
				if !ok || p.Status != http.StatusBadRequest || !errors.Is(p.Cause(), ErrInvalidCursor) {
					t.Fatalf("err = %v, want 400 problem caused by ErrInvalidCursor", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if (key == nil) != tc.wantNil {
				t.Errorf("key = %v", key)
			}
		})
	}
}

func TestNewPage(t *testing.T) {
	empty := NewPage[int](nil, nil, "o")
	raw, _ := json.Marshal(empty)
	if string(raw) != `{"data":[],"next_cursor":null}` {
		t.Errorf("empty page = %s", raw)
	}
	p := NewPage([]int{1}, map[string]types.AttributeValue{"pk": s("a")}, "o")
	if p.NextCursor == nil {
		t.Fatal("want next cursor")
	}
	if _, err := DecodeCursor(*p.NextCursor, "o"); err != nil {
		t.Fatal(err)
	}
}

// Walks three pages forward, then follows previous_cursor back, asserting the
// start key each cursor resolves to.
func TestPageWithPreviousNavigation(t *testing.T) {
	const owner = "org-1"
	k := func(v string) map[string]types.AttributeValue { return map[string]types.AttributeValue{"pk": s(v)} }
	startOf := func(c *string) string {
		key, err := DecodeCursor(*c, owner)
		if err != nil {
			t.Fatal(err)
		}
		return key["pk"].(*types.AttributeValueMemberS).Value
	}

	p1 := NewPageWithPrevious([]int{1}, k("end1"), "", owner)
	if p1.HasPrevious || p1.PreviousCursor != nil || !p1.HasNext {
		t.Fatalf("page1 = %+v", p1)
	}
	p2 := NewPageWithPrevious([]int{2}, k("end2"), *p1.NextCursor, owner) // fetched from end1
	if !p2.HasPrevious || p2.PreviousCursor != nil {
		t.Fatalf("page2 = %+v (previous of page 2 is page 1: no cursor)", p2)
	}
	p3 := NewPageWithPrevious([]int{3}, k("end3"), *p2.NextCursor, owner) // fetched from end2
	if !p3.HasPrevious || p3.PreviousCursor == nil || startOf(p3.PreviousCursor) != "end1" {
		t.Fatalf("page3 previous must start page 2 (end1): %+v", p3)
	}
	last := NewPageWithPrevious([]int{4}, nil, *p3.NextCursor, owner)
	if last.HasNext || last.NextCursor != nil {
		t.Fatalf("last = %+v", last)
	}
	// the cursor does not grow with depth
	if len(*p3.NextCursor) > len(*p2.NextCursor)+60 {
		t.Errorf("cursor grows: %d vs %d", len(*p3.NextCursor), len(*p2.NextCursor))
	}
	// previous_cursor is itself a valid cursor for this owner only
	if _, err := DecodeCursor(*p3.PreviousCursor, "other"); err == nil {
		t.Error("previous cursor leaked across owners")
	}
	raw, _ := json.Marshal(NewPageWithPrevious[int](nil, nil, "", owner))
	if !strings.Contains(string(raw), `"data":[]`) {
		t.Errorf("empty data must be [], got %s", raw)
	}
}
