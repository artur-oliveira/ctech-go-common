// Package problem implements RFC 7807 Problem Details, shared across CTech
// services so every service in the platform emits consistent error bodies.
// Service-specific problem types (e.g. fiscal, wallet) live in each consumer's
// own problem package, built on top of the generic constructors here.
package problem

import (
	"net/http"
	"time"
)

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
	TypeServiceUnavailable  = "/problems/service-unavailable"
	// TypeTimeout is a request cut by its time budget (REQUEST_TIMEOUT): the
	// 504 every product already emits under this URI.
	TypeTimeout            = "/problems/timeout"
	TypeNotImplemented     = "/problems/not-implemented"
	TypePayloadTooLarge    = "/problems/payload-too-large"
	TypePaymentRequired    = "/problems/payment-required"
	TypePreconditionFailed = "/problems/precondition-failed"
	// TypeHTTPError is a 4xx *fiber.Error with no more specific type (see FromError).
	TypeHTTPError = "/problems/http-error"
)

// FieldError describes a single field-level validation failure.
type FieldError struct {
	Field   string `json:"field"`         // dotted JSON path, e.g. "person.addresses[0].postal_code"
	Message string `json:"message"`       // human-readable message
	Tag     string `json:"tag,omitempty"` // validation rule that failed, e.g. "required", "cnpj"
	// Code is the stable machine-readable reason ("required", "too_long") a
	// client maps to its own translated text; Params carries the numbers prose
	// would embed (e.g. {"max": 64}). Message stays an English fallback.
	Code   string         `json:"code,omitempty"`
	Params map[string]any `json:"params,omitempty"`
}

// Field builds a FieldError with a Code. params is an alternating key, value
// list; a pair whose key is not a string, and a trailing odd value, are ignored.
func Field(field, code, message string, params ...any) FieldError {
	fe := FieldError{Field: field, Code: code, Message: message}
	for i := 0; i+1 < len(params); i += 2 {
		k, ok := params[i].(string)
		if !ok {
			continue
		}
		if fe.Params == nil {
			fe.Params = map[string]any{}
		}
		fe.Params[k] = params[i+1]
	}
	return fe
}

// NextAction values tell a client what it can DO about a problem, instead of
// leaving it to pattern-match on Detail. The set is closed on purpose: a
// client switches on it, so a service inventing a seventh value silently
// falls into the client's default branch.
const (
	// NextActionRetry: the same request may be repeated as-is, after
	// RetryAfterSeconds if it is set.
	NextActionRetry = "retry"
	// NextActionWait: the condition clears on its own, but repeating the
	// request is not what clears it (a full table, a queued job).
	NextActionWait = "wait"
	// NextActionReauthenticate: the caller's credentials are the problem —
	// refresh the token or sign in again.
	NextActionReauthenticate = "reauthenticate"
	// NextActionContactSupport: nothing the client can do unattended.
	NextActionContactSupport = "contact_support"
)

// Problem is an RFC 7807 Problem Details response body. Errors carries field
// failures (only populated for validation problems; omitted otherwise).
// MaxAgeSeconds carries the step-up freshness window on step-up-required
// problems. MinAmount/MaxAmount carry the accepted range on out-of-range
// problems so the UI can state the bounds without hardcoding them.
// NextAction/RetryAfterSeconds carry structured recovery guidance (see
// WithNextAction). All of them are optional extension fields — omitted unless
// a specific problem sets them.
type Problem struct {
	Type          string       `json:"type"`
	Title         string       `json:"title"`
	Status        int          `json:"status"`
	Detail        string       `json:"detail,omitempty"`
	Errors        []FieldError `json:"errors,omitempty"`
	MaxAgeSeconds int          `json:"max_age_seconds,omitempty"`
	MinAmount     int64        `json:"min_amount,omitempty"`
	MaxAmount     int64        `json:"max_amount,omitempty"`
	// NextAction is one of the NextAction* constants above: what the client
	// should do next. RetryAfterSeconds is how long it should wait first,
	// mirroring the Retry-After header's delay-seconds form. Both are RFC
	// 9457 extension members, so a client that ignores them is unaffected.
	NextAction        string `json:"next_action,omitempty"`
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
	// Instance identifies this occurrence (RFC 9457 "instance"), e.g. the path.
	Instance string `json:"instance,omitempty"`
	// Code is the stable machine-readable identifier of the specific failure
	// (WithCode); a client branches on it, never on Detail. Registry.New fills
	// it from the type slug.
	Code string `json:"code,omitempty"`
	// RequiredScope names the permission a 403 on a protected resource needs.
	RequiredScope string `json:"required_scope,omitempty"`

	// extras are product-specific extension members (WithExtra). Unexported so
	// embedding structs keep marshalling with plain encoding/json; Send and
	// MarshalBody merge them into the body.
	extras map[string]any

	// cause is available to the HTTP logging boundary but is never serialized.
	cause error
}

func (p *Problem) Error() string {
	if p.Detail != "" {
		return p.Title + ": " + p.Detail
	}
	return p.Title
}

// WithCause preserves an internal failure without exposing it in the RFC 7807
// response. Consumer wrappers should return their own type after delegating to
// this method when fluent chaining is needed.
func (p *Problem) WithCause(err error) *Problem {
	p.cause = err
	return p
}

// Cause returns the internal error associated with this safe public problem.
func (p *Problem) Cause() error { return p.cause }

// WithNextAction attaches structured recovery guidance: what the client can do
// (one of the NextAction* constants) and, for the actions where waiting is
// part of it, how many seconds to wait. Pass 0 seconds when there is no
// meaningful delay to advertise — a fabricated one is worse than none, since
// clients back off on it.
//
// Only set this where the SERVICE actually knows the answer (a rate limiter
// knows its window; an expired token knows re-auth is the fix). A guess here
// becomes a client's retry loop.
func (p *Problem) WithNextAction(action string, retryAfterSeconds int) *Problem {
	p.NextAction = action
	if retryAfterSeconds > 0 {
		p.RetryAfterSeconds = retryAfterSeconds
	}
	return p
}

// WithCode names the specific failure with a stable machine-readable code.
func (p *Problem) WithCode(code string) *Problem {
	p.Code = code
	return p
}

// WithInstance sets the RFC 9457 instance member.
func (p *Problem) WithInstance(instance string) *Problem {
	p.Instance = instance
	return p
}

// WithRequiredScope sets the permission a 403 needs (required_scope).
func (p *Problem) WithRequiredScope(scope string) *Problem {
	p.RequiredScope = scope
	return p
}

// WithMaxAge sets max_age_seconds, the step-up freshness window, rounded down
// to whole seconds.
func (p *Problem) WithMaxAge(d time.Duration) *Problem {
	p.MaxAgeSeconds = int(d / time.Second)
	return p
}

// WithErrors attaches field-level errors (errors[]).
func (p *Problem) WithErrors(errs ...FieldError) *Problem {
	p.Errors = errs
	return p
}

// WithExtra adds an arbitrary extension member (e.g. "blockers", "quota").
// It cannot replace a standard member: on a key clash the typed field wins.
// Only MarshalBody and Send include extras; json.Marshal(p) does not.
func (p *Problem) WithExtra(key string, value any) *Problem {
	if p.extras == nil {
		p.extras = map[string]any{}
	}
	p.extras[key] = value
	return p
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

// TooManyRequests is always retryable by definition — the caller did nothing
// wrong except go too fast — so it carries NextActionRetry without the caller
// having to remember. retryAfterSeconds is optional (0 = unknown window);
// TooManyRequestsAfter is the form to use when the limiter knows its window.
func TooManyRequests(detail string) *Problem {
	return TooManyRequestsAfter(detail, 0)
}

// TooManyRequestsAfter is TooManyRequests carrying the limiter's own window,
// so the client waits exactly as long as the limit lasts instead of guessing.
func TooManyRequestsAfter(detail string, retryAfterSeconds int) *Problem {
	return New(http.StatusTooManyRequests, TypeTooManyRequests, "Too Many Requests", detail).
		WithNextAction(NextActionRetry, retryAfterSeconds)
}

func InternalServer(detail string) *Problem {
	return New(http.StatusInternalServerError, TypeInternalServer, "Internal Server Error", detail)
}

// UnprocessableEntityFields is a 422 with a detail and field-level errors,
// under the generic unprocessable-entity type. Validation is the same without
// a detail, under the validation-error type.
func UnprocessableEntityFields(detail string, errs ...FieldError) *Problem {
	return UnprocessableEntity(detail).WithErrors(errs...)
}

// ServiceUnavailable is a 503: a dependency did not answer and nothing was
// decided. Retryable, so it carries NextActionRetry.
func ServiceUnavailable(detail string) *Problem {
	return New(http.StatusServiceUnavailable, TypeServiceUnavailable, "Service Unavailable", detail).
		WithNextAction(NextActionRetry, 0)
}

// GatewayTimeout is a 504: the request used its whole time budget. Retryable;
// for a write the outcome is unknown until it is read back.
func GatewayTimeout(detail string) *Problem {
	return New(http.StatusGatewayTimeout, TypeTimeout, "Gateway Timeout", detail).
		WithNextAction(NextActionRetry, 0)
}

func NotImplemented(detail string) *Problem {
	return New(http.StatusNotImplemented, TypeNotImplemented, "Not Implemented", detail)
}

func PayloadTooLarge(detail string) *Problem {
	return New(http.StatusRequestEntityTooLarge, TypePayloadTooLarge, "Payload Too Large", detail)
}

// PaymentRequired is a 402: the caller is allowed, what is missing is a
// subscription or quota. Name the reason with WithCode.
func PaymentRequired(detail string) *Problem {
	return New(http.StatusPaymentRequired, TypePaymentRequired, "Payment Required", detail)
}

func PreconditionFailed(detail string) *Problem {
	return New(http.StatusPreconditionFailed, TypePreconditionFailed, "Precondition Failed", detail)
}
