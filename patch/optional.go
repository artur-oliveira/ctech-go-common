// Package patch holds what a partial update (an HTTP PATCH body) needs and Go's
// pointers do not give: a field that can be absent, null or a value.
//
// The rule: an explicit JSON null clears an optional field; absent keeps it;
// a value replaces it. A *T cannot carry that rule, because absent and null
// both decode to nil. Clearing a required field is the handler's call (a 422,
// say), never silently ignored: IsNull tells the handler the client asked.
//
// Optional is decode-only. It has no MarshalJSON, so encoding one yields {}
// (its fields are unexported) and an omitempty tag does not omit it; build
// response bodies from plain types. Decode every body into a fresh value:
// json.Unmarshal leaves a key the body does not name untouched, so a reused
// struct still carries the previous body's state. When the key appears more
// than once, the last occurrence wins, null included. When json.Unmarshal
// returns an error the body must be rejected as a whole; the Optional that
// failed is left marked present with whatever T's decoder had filled in.
//
// Extracted from ctech-billing (api/internal/patch), which switches by changing
// only its import path.
package patch

import (
	"bytes"
	"encoding/json"
)

// Optional is one field of a PATCH body. The zero value is "absent", so a
// struct of Optionals decodes a body that names none of them as "change
// nothing".
type Optional[T any] struct {
	present bool
	null    bool
	value   T
}

// Of is a field that carries v.
func Of[T any](v T) Optional[T] { return Optional[T]{present: true, value: v} }

// Null is a field sent as JSON null: clear it.
func Null[T any]() Optional[T] { return Optional[T]{present: true, null: true} }

// UnmarshalJSON is called for every key that is in the body, null included
// (encoding/json hands a non-pointer Unmarshaler the literal null), so a key
// that never reaches it stayed absent. The value is decoded by T's own rules.
func (o *Optional[T]) UnmarshalJSON(b []byte) error {
	o.present = true
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		o.null, o.value = true, *new(T)
		return nil
	}
	o.null = false
	return json.Unmarshal(b, &o.value)
}

// Present says the body named the field, with a value or null.
func (o Optional[T]) Present() bool { return o.present }

// IsNull says the body asked to clear the field.
func (o Optional[T]) IsNull() bool { return o.present && o.null }

// Get is the value, and whether there is one (false when absent or null).
func (o Optional[T]) Get() (T, bool) { return o.value, o.present && !o.null }

// Ptr is the value as a pointer, nil when absent or null: what a handler hands
// to a repository patch whose nil already means "keep". The pointer is to a
// copy, so writing through it does not change the Optional.
func (o Optional[T]) Ptr() *T {
	if !o.present || o.null {
		return nil
	}
	v := o.value
	return &v
}
