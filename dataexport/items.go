package dataexport

import (
	"archive/zip"
	"encoding/json"
	"io"
	"strconv"
)

// Items writes one zip entry as {"items":[...],"truncated":bool}, one item at a
// time, so a person's ledger or purchase history never has to sit in memory to be
// exported. An entry cannot be rewritten, so whether the list was cut is the last
// field, written by End.
type Items struct {
	w io.Writer
	n int
}

// BeginItems starts the entry name in zw. Call Add for each item, then End.
func BeginItems(zw *zip.Writer, name string) (*Items, error) {
	w, err := zw.Create(name)
	if err != nil {
		return nil, err
	}
	if _, err := io.WriteString(w, `{"items":[`); err != nil {
		return nil, err
	}
	return &Items{w: w}, nil
}

// Add marshals one item into the entry.
func (i *Items) Add(item any) error {
	data, err := json.Marshal(item)
	if err != nil {
		return err
	}
	sep := "\n"
	if i.n > 0 {
		sep = ",\n"
	}
	i.n++
	if _, err := io.WriteString(i.w, sep); err != nil {
		return err
	}
	_, err = i.w.Write(data)
	return err
}

// Count is how many items were added.
func (i *Items) Count() int { return i.n }

// End closes the list; truncated says the source had more than was written.
func (i *Items) End(truncated bool) error {
	_, err := io.WriteString(i.w, "\n],\"truncated\":"+strconv.FormatBool(truncated)+"}\n")
	return err
}
