package validation

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"gopkg.aoctech.app/api-commons/problem"
)

// DefaultMaxBodyBytes caps a JSON body (1 MiB). Fail-closed: a larger body is
// rejected before it is parsed.
const DefaultMaxBodyBytes = 1 << 20

const (
	detailInvalidJSON = "invalid JSON body"
	detailTooLarge    = "request body too large"
)

// BindJSON strictly decodes body into dst and validates it. Unknown fields,
// trailing data and bodies over DefaultMaxBodyBytes are rejected. The client
// message is generic and never echoes the parse error (it can reveal struct
// field names) or the body (PII); the real cause is logged server-side only.
func BindJSON[T any](body []byte, dst *T) *problem.Problem {
	return BindJSONLimit(body, dst, DefaultMaxBodyBytes)
}

// BindJSONLimit is BindJSON with an explicit size limit in bytes.
func BindJSONLimit[T any](body []byte, dst *T, maxBytes int) *problem.Problem {
	if len(body) > maxBytes {
		return problem.New(http.StatusRequestEntityTooLarge, problem.TypeBadRequest, "Payload Too Large", detailTooLarge)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	err := dec.Decode(dst)
	if err == nil {
		if _, tail := dec.Token(); !errors.Is(tail, io.EOF) {
			err = errors.New("trailing data after JSON value")
		}
	}
	if err != nil {
		slog.Warn("bind json failed", "err", err)
		return problem.BadRequest(detailInvalidJSON).WithCause(err)
	}
	return Struct(dst)
}
