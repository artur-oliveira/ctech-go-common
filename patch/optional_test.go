package patch

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// date is a T with its own decoding rules (strict YYYY-MM-DD), standing in for
// the domain types consumers wrap (billing's brcal.Date, for one).
type date struct{ time.Time }

func (d *date) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return err
	}
	d.Time = t
	return nil
}

type body struct {
	End  Optional[date]   `json:"end"`
	Note Optional[string] `json:"note"`
	Day  Optional[int]    `json:"day"`
}

func decode(t *testing.T, raw string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(raw), v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
}

func wantAbsent[T any](t *testing.T, name string, o Optional[T]) {
	t.Helper()
	if o.Present() || o.IsNull() || o.Ptr() != nil {
		t.Fatalf("%s = %+v, want absent", name, o)
	}
	if _, ok := o.Get(); ok {
		t.Fatalf("%s: absent has a value", name)
	}
}

func wantNull[T any](t *testing.T, name string, o Optional[T]) {
	t.Helper()
	if !o.Present() || !o.IsNull() || o.Ptr() != nil {
		t.Fatalf("%s = %+v, want present and null", name, o)
	}
	if _, ok := o.Get(); ok {
		t.Fatalf("%s: null has a value", name)
	}
}

func wantValue[T comparable](t *testing.T, name string, o Optional[T], want T) {
	t.Helper()
	v, ok := o.Get()
	if !ok || v != want || !o.Present() || o.IsNull() {
		t.Fatalf("%s = %+v, want the value %v", name, o, want)
	}
	if p := o.Ptr(); p == nil || *p != want {
		t.Fatalf("%s ptr = %v, want %v", name, p, want)
	}
}

// A PATCH body has three states per field, and Go's *T has two: absent and
// null both decode to nil. Optional tells them apart.
func TestOptionalTellsAbsentFromNullFromAValue(t *testing.T) {
	var b body
	decode(t, `{"end":null,"note":"x"}`, &b)
	wantNull(t, "end", b.End)
	wantValue(t, "note", b.Note, "x")
	wantAbsent(t, "day", b.Day)
}

func TestAnEmptyBodyChangesNothing(t *testing.T) {
	var b body
	decode(t, `{}`, &b)
	wantAbsent(t, "end", b.End)
	wantAbsent(t, "note", b.Note)
	wantAbsent(t, "day", b.Day)
}

// The zero value of T is a value, not absence: 0, "" and false are what the
// client asked to store.
func TestTheZeroValueOfTIsAValueNotAbsence(t *testing.T) {
	var b body
	decode(t, `{"day":0,"note":""}`, &b)
	wantValue(t, "day", b.Day, 0)
	wantValue(t, "note", b.Note, "")

	var f struct {
		On Optional[bool] `json:"on"`
	}
	decode(t, `{"on":false}`, &f)
	wantValue(t, "on", f.On, false)

	if v, ok := (Optional[int]{}).Get(); ok || v != 0 {
		t.Fatalf("zero Optional Get = %v, %v; want 0, false", v, ok)
	}
	wantValue(t, "Of(0)", Of(0), 0)
}

// Only the JSON literal null clears; the string "null" is a value.
func TestTheStringNullIsAValue(t *testing.T) {
	var b body
	decode(t, `{"note":"null"}`, &b)
	wantValue(t, "note", b.Note, "null")
}

func TestNullIsRecognisedThroughWhitespace(t *testing.T) {
	var b body
	decode(t, "{\"day\" :\n\t null \n}", &b)
	wantNull(t, "day", b.Day)

	var o Optional[int]
	if err := o.UnmarshalJSON([]byte("  null\n")); err != nil {
		t.Fatal(err)
	}
	wantNull(t, "direct", o)
}

func TestOptionalDecodesItsValueThroughTheTypesOwnRules(t *testing.T) {
	var b body
	decode(t, `{"end":"2026-05-10"}`, &b)
	if v, ok := b.End.Get(); !ok || !v.Equal(time.Date(2026, time.May, 10, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("end = %+v", b.End)
	}
	var b2 body
	if err := json.Unmarshal([]byte(`{"end":"10/05/2026"}`), &b2); err == nil {
		t.Fatal("a malformed date was accepted")
	}
}

func TestAWrongTypeIsAnError(t *testing.T) {
	cases := []string{
		`{"day":"3"}`,
		`{"day":1.5}`,
		`{"day":true}`,
		`{"note":3}`,
		`{"note":{}}`,
		`{"end":3}`,
	}
	for _, raw := range cases {
		var b body
		err := json.Unmarshal([]byte(raw), &b)
		if err == nil {
			t.Errorf("%s: accepted", raw)
			continue
		}
		var typeErr *json.UnmarshalTypeError
		if raw != `{"end":3}` && !errors.As(err, &typeErr) {
			t.Errorf("%s: err = %T %v, want *json.UnmarshalTypeError", raw, err, err)
		}
	}
}

// encoding/json lets the last occurrence of a key win; Optional follows,
// in both directions between null and a value.
func TestADuplicateKeyLastOneWins(t *testing.T) {
	var b body
	decode(t, `{"note":"x","note":null}`, &b)
	wantNull(t, "note value-then-null", b.Note)

	var b2 body
	decode(t, `{"note":null,"note":"y"}`, &b2)
	wantValue(t, "note null-then-value", b2.Note, "y")

	var b3 body
	decode(t, `{"day":1,"day":2}`, &b3)
	wantValue(t, "day", b3.Day, 2)
}

type address struct {
	City Optional[string] `json:"city"`
	Zip  Optional[string] `json:"zip"`
}

type nested struct {
	Address Optional[address] `json:"address"`
	Contact struct {
		Phone Optional[string] `json:"phone"`
	} `json:"contact"`
	Tags Optional[[]string] `json:"tags"`
}

func TestNestedStructFields(t *testing.T) {
	var n nested
	decode(t, `{"address":{"city":null},"contact":{"phone":"123"}}`, &n)
	a, ok := n.Address.Get()
	if !ok {
		t.Fatalf("address = %+v, want a value", n.Address)
	}
	wantNull(t, "address.city", a.City)
	wantAbsent(t, "address.zip", a.Zip)
	wantValue(t, "contact.phone", n.Contact.Phone, "123")
	wantAbsent(t, "tags", n.Tags)

	var cleared nested
	decode(t, `{"address":null,"contact":{}}`, &cleared)
	wantNull(t, "address", cleared.Address)
	wantAbsent(t, "contact.phone", cleared.Contact.Phone)

	var empty nested
	decode(t, `{"address":{}}`, &empty)
	a, ok = empty.Address.Get()
	if !ok {
		t.Fatal("an empty object is a value")
	}
	wantAbsent(t, "address.city", a.City)
}

// An empty list is a value (replace with nothing); null clears.
func TestAnEmptyListIsAValueNotNull(t *testing.T) {
	var n nested
	decode(t, `{"tags":[]}`, &n)
	v, ok := n.Tags.Get()
	if !ok || v == nil || len(v) != 0 || n.Tags.IsNull() {
		t.Fatalf("tags = %+v, want an empty non-nil list", n.Tags)
	}
	var m nested
	decode(t, `{"tags":null}`, &m)
	wantNull(t, "tags", m.Tags)
}

// Optional[*T] keeps null at the Optional level: the inner pointer is not
// what says "clear".
func TestOptionalOfAPointer(t *testing.T) {
	var f struct {
		P Optional[*int] `json:"p"`
	}
	decode(t, `{"p":null}`, &f)
	wantNull(t, "p", f.P)
	decode(t, `{"p":4}`, &f)
	if v, ok := f.P.Get(); !ok || v == nil || *v != 4 {
		t.Fatalf("p = %+v", f.P)
	}
}

// omitempty only affects encoding, so it changes nothing about decoding.
// Optional has no MarshalJSON: encoded, it is {} whatever its state, and
// omitempty does not omit a struct. Responses must not be built from it.
func TestOmitemptyInterplay(t *testing.T) {
	type tagged struct {
		Note Optional[string] `json:"note,omitempty"`
		Day  Optional[int]    `json:"day,omitempty"`
	}
	var b tagged
	decode(t, `{"note":null}`, &b)
	wantNull(t, "note", b.Note)
	wantAbsent(t, "day", b.Day)

	for _, v := range []tagged{{}, {Note: Of("x"), Day: Null[int]()}} {
		out, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != `{"note":{},"day":{}}` {
			t.Fatalf("marshal %+v = %s, want {\"note\":{},\"day\":{}}", v, out)
		}
	}
}

// json.Unmarshal does not reset a key the body omits, so a reused struct keeps
// the last body's state. Decode every body into a fresh value.
func TestAReusedStructKeepsThePreviousBodysState(t *testing.T) {
	var b body
	decode(t, `{"note":"x"}`, &b)
	decode(t, `{"day":1}`, &b)
	wantValue(t, "note", b.Note, "x")
	wantValue(t, "day", b.Day, 1)
}

// Decoding null into an Optional that held a value drops the old value, so
// Get on a reused Optional never leaks it.
func TestNullDropsAPreviousValue(t *testing.T) {
	o := Of("old")
	if err := json.Unmarshal([]byte(`null`), &o); err != nil {
		t.Fatal(err)
	}
	wantNull(t, "o", o)
	if v, _ := o.Get(); v != "" {
		t.Fatalf("null kept the value %q", v)
	}
}

func TestPtrIsNilUnlessThereIsAValueAndIsACopy(t *testing.T) {
	var b body
	decode(t, `{"day":null,"note":"y"}`, &b)
	if b.Day.Ptr() != nil {
		t.Fatal("a null day has a pointer")
	}
	p := b.Note.Ptr()
	if p == nil || *p != "y" {
		t.Fatalf("note ptr = %v", p)
	}
	*p = "z"
	wantValue(t, "note after write through Ptr", b.Note, "y")
	if (Optional[int]{}).Ptr() != nil {
		t.Fatal("an absent day has a pointer")
	}
}

func TestOfAndNullBuildTheStatesForCallers(t *testing.T) {
	wantValue(t, "Of(3)", Of(3), 3)
	wantNull(t, "Null", Null[int]())
	wantAbsent(t, "zero", Optional[int]{})
}
