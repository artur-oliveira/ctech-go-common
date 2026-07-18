// Package problem implements RFC 7807 Problem Details, shared across CTech
// services so every service in the platform emits consistent error bodies.
// Service-specific problem types (e.g. fiscal, wallet) live in each consumer's
// own problem package, built on top of the generic constructors here.
package problem

import "net/http"

const (
	TypeBadRequest          = "/problems/bad-request"
	TypeUnauthorized        = "/problems/unauthorized"
	TypeForbidden           = "/problems/forbidden"
	TypeNotFound            = "/problems/not-found"
	TypeConflict            = "/problems/conflict"
	TypeUnprocessableEntity = "/problems/unprocessable-entity"
	TypeValidation          = "/problems/validation-error"
	TypeTooManyRequests     = "/problems/too-many-requests"
	TypeInternalServer      = "/problems/internal-server-error"
)

// FieldError describes a single field-level validation failure.
type FieldError struct {
	Field   string `json:"field"`         // dotted JSON path, e.g. "person.addresses[0].postal_code"
	Message string `json:"message"`       // human-readable message
	Tag     string `json:"tag,omitempty"` // validation rule that failed, e.g. "required", "cnpj"
}

// Problem is an RFC 7807 Problem Details response body. Errors carries field
// failures (only populated for validation problems; omitted otherwise).
// MaxAgeSeconds carries the step-up freshness window on step-up-required
// problems. MinAmount/MaxAmount carry the accepted range on out-of-range
// problems so the UI can state the bounds without hardcoding them. All three
// are optional extension fields — omitted unless a specific problem sets them.
type Problem struct {
	Type          string       `json:"type"`
	Title         string       `json:"title"`
	Status        int          `json:"status"`
	Detail        string       `json:"detail,omitempty"`
	Errors        []FieldError `json:"errors,omitempty"`
	MaxAgeSeconds int          `json:"max_age_seconds,omitempty"`
	MinAmount     int64        `json:"min_amount,omitempty"`
	MaxAmount     int64        `json:"max_amount,omitempty"`
}

func (p *Problem) Error() string {
	if p.Detail != "" {
		return p.Title + ": " + p.Detail
	}
	return p.Title
}

// New builds a Problem with the given status, type URI, title, and detail.
func New(status int, typ, title, detail string) *Problem {
	return &Problem{Type: typ, Title: title, Status: status, Detail: detail}
}

func BadRequest(detail string) *Problem {
	return New(http.StatusBadRequest, TypeBadRequest, "Bad Request", detail)
}

func Unauthorized(detail string) *Problem {
	return New(http.StatusUnauthorized, TypeUnauthorized, "Unauthorized", detail)
}

func Forbidden(detail string) *Problem {
	return New(http.StatusForbidden, TypeForbidden, "Forbidden", detail)
}

func NotFound(detail string) *Problem {
	return New(http.StatusNotFound, TypeNotFound, "Not Found", detail)
}

func Conflict(detail string) *Problem {
	return New(http.StatusConflict, TypeConflict, "Conflict", detail)
}

func UnprocessableEntity(detail string) *Problem {
	return New(http.StatusUnprocessableEntity, TypeUnprocessableEntity, "Unprocessable Entity", detail)
}

// Validation returns a 422 problem carrying the given field-level errors.
// Used by the request-binding layer when a request body fails struct validation.
func Validation(errs []FieldError) *Problem {
	p := New(http.StatusUnprocessableEntity, TypeValidation, "Validation Error", "")
	p.Errors = errs
	return p
}

func TooManyRequests(detail string) *Problem {
	return New(http.StatusTooManyRequests, TypeTooManyRequests, "Too Many Requests", detail)
}

func InternalServer(detail string) *Problem {
	return New(http.StatusInternalServerError, TypeInternalServer, "Internal Server Error", detail)
}
