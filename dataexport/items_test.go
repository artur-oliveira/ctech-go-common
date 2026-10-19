package dataexport

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"testing"
)

func readEntry(t *testing.T, buf *bytes.Buffer, name string) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		if f.Name == name {
			r, _ := f.Open()
			defer r.Close()
			b, _ := io.ReadAll(r)
			return b
		}
	}
	t.Fatalf("no entry %s", name)
	return nil
}

func TestItemsWritesValidJSONWhateverTheCount(t *testing.T) {
	for _, tc := range []struct {
		n   int
		cut bool
	}{{0, false}, {1, false}, {3, true}} {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		it, err := BeginItems(zw, "ledger.json")
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < tc.n; i++ {
			if err := it.Add(map[string]int{"n": i}); err != nil {
				t.Fatal(err)
			}
		}
		if it.Count() != tc.n {
			t.Fatalf("count = %d", it.Count())
		}
		if err := it.End(tc.cut); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		var got struct {
			Items     []map[string]int `json:"items"`
			Truncated bool             `json:"truncated"`
		}
		if err := json.Unmarshal(readEntry(t, &buf, "ledger.json"), &got); err != nil {
			t.Fatalf("n=%d: %v", tc.n, err)
		}
		if len(got.Items) != tc.n || got.Truncated != tc.cut {
			t.Fatalf("n=%d: %+v", tc.n, got)
		}
	}
}
