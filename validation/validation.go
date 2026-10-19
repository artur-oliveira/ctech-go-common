// Package validation is the shared request-validation layer: one
// go-playground/validator singleton (field paths use json tag names), stable
// error codes instead of free text, conversion to an RFC 7807 422 problem, a
// strict JSON binder and the validators every CTech service needs (CPF, CNPJ,
// phone, zip, ULID/UUID, money in cents). It never touches HTTP frameworks.
package validation

import (
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync"

	"github.com/go-playground/validator/v10"

	"gopkg.aoctech.app/api-commons/problem"
)

// Stable error codes. A client switches on these (and on Param), never on
// Message, which is a default English rendering of the code.
const (
	CodeRequired    = "required"
	CodeTooShort    = "too_short"
	CodeTooLong     = "too_long"
	CodeOutOfRange  = "out_of_range"
	CodeInvalidEnum = "invalid_value"
	CodeInvalidList = "invalid_list"
	CodeInvalidFmt  = "invalid_format"
	CodeInvalid     = "invalid"

	CodeInvalidEmail = "invalid_email"
	CodeInvalidURL   = "invalid_url"
	CodeInvalidCPF   = "invalid_cpf"
	CodeInvalidCNPJ  = "invalid_cnpj"
	CodeInvalidTaxID = "invalid_tax_id"
	CodeInvalidPhone = "invalid_phone"
	CodeInvalidZip   = "invalid_zip"
	CodeInvalidULID  = "invalid_ulid"
	CodeInvalidUUID  = "invalid_uuid"
	CodeInvalidMoney = "invalid_money"
)

// Tags of the validators this package registers (plus the stock ulid/uuid).
const (
	TagCPF        = "cpf"
	TagCNPJ       = "cnpj"
	TagCPFCNPJ    = "cpfcnpj"
	TagPhone      = "phone"
	TagZip        = "zip"
	TagMoneyCents = "money_cents"
	tagULID       = "ulid"
	tagUUID       = "uuid"
)

// tagCodes maps a validator tag to its stable code. Tags not listed (and not
// added with Register) surface as CodeInvalid, with the tag in FieldError.Tag.
var tagCodes = map[string]string{
	"required": CodeRequired, "required_if": CodeRequired, "required_with": CodeRequired,
	"required_without": CodeRequired, "required_unless": CodeRequired,
	"min": CodeTooShort, "max": CodeTooLong, "len": CodeOutOfRange,
	"gt": CodeOutOfRange, "gte": CodeOutOfRange, "lt": CodeOutOfRange, "lte": CodeOutOfRange,
	"oneof": CodeInvalidEnum, "dive": CodeInvalidList, "unique": CodeInvalidList,
	"email": CodeInvalidEmail, "url": CodeInvalidURL, "http_url": CodeInvalidURL,
	"numeric": CodeInvalidFmt, "alphanum": CodeInvalidFmt, "alpha": CodeInvalidFmt,
	TagCPF: CodeInvalidCPF, TagCNPJ: CodeInvalidCNPJ, TagCPFCNPJ: CodeInvalidTaxID,
	TagPhone: CodeInvalidPhone, TagZip: CodeInvalidZip,
	tagULID: CodeInvalidULID, tagUUID: CodeInvalidUUID, TagMoneyCents: CodeInvalidMoney,
}

// messages are the default English texts per code; unknown codes fall back to
// the CodeInvalid text. Consumers localize by code.
var messages = map[string]string{
	CodeRequired:     "required",
	CodeTooShort:     "too short or too small",
	CodeTooLong:      "too long or too large",
	CodeOutOfRange:   "out of range",
	CodeInvalidEnum:  "not an accepted value",
	CodeInvalidList:  "invalid list",
	CodeInvalidFmt:   "invalid format",
	CodeInvalid:      "invalid value",
	CodeInvalidEmail: "invalid email address",
	CodeInvalidURL:   "invalid URL",
	CodeInvalidCPF:   "invalid CPF",
	CodeInvalidCNPJ:  "invalid CNPJ",
	CodeInvalidTaxID: "invalid CPF or CNPJ",
	CodeInvalidPhone: "invalid phone number",
	CodeInvalidZip:   "invalid zip code",
	CodeInvalidULID:  "invalid ULID",
	CodeInvalidUUID:  "invalid UUID",
	CodeInvalidMoney: "invalid amount",
}

// FieldError is one failed rule. It is deliberately local (not problem.FieldError)
// so this package does not depend on that type's evolving shape; ToProblem is the
// single place that adapts it.
type FieldError struct {
	Field string // dotted JSON path, e.g. "items[0].amount"
	Code  string // stable Code* value
	Tag   string // validator rule that failed, e.g. "max"
	Param string // rule parameter, e.g. "64" for max=64
}

// Message is the default English text for the error's Code.
func (e FieldError) Message() string {
	if m, ok := messages[e.Code]; ok {
		return m
	}
	return messages[CodeInvalid]
}

// ValidationError is returned by Validate when the input broke one or more rules.
type ValidationError struct{ Fields []FieldError }

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Fields))
	for _, f := range e.Fields {
		parts = append(parts, f.Field+": "+f.Code)
	}
	return "validation failed: " + strings.Join(parts, "; ")
}

// IsValidationError reports whether err is (or wraps) a *ValidationError.
func IsValidationError(err error) (*ValidationError, bool) {
	return errors.AsType[*ValidationError](err)
}

var (
	instance *validator.Validate
	once     sync.Once
	mu       sync.Mutex // guards tagCodes in Register
)

func get() *validator.Validate {
	once.Do(func() {
		v := validator.New(validator.WithRequiredStructEnabled())
		v.RegisterTagNameFunc(func(f reflect.StructField) string {
			name := strings.SplitN(f.Tag.Get("json"), ",", 2)[0]
			if name == "-" || name == "" {
				return f.Name
			}
			return name
		})
		for tag, fn := range builtinValidators {
			if err := v.RegisterValidation(tag, fn); err != nil {
				panic("validation: register " + tag + ": " + err.Error()) // programmer error at init
			}
		}
		instance = v
	})
	return instance
}

// Register adds a service-specific rule (e.g. a fiscal CFOP) to the shared
// validator and maps its tag to a stable code. Call it from init/main before
// serving requests; validator.RegisterValidation is not safe against concurrent
// validation. Re-registering an existing tag is rejected.
func Register(tag string, fn validator.Func, code string) error {
	mu.Lock()
	defer mu.Unlock()
	if _, taken := tagCodes[tag]; taken {
		return errors.New("validation: tag already registered: " + tag)
	}
	if err := get().RegisterValidation(tag, fn); err != nil {
		return err
	}
	tagCodes[tag] = code
	return nil
}

// Validate checks s (a struct or pointer to one). It returns nil when valid,
// *ValidationError for rule failures, and the raw error for programmer errors
// (e.g. a non-struct), which callers must treat as a 500.
func Validate(s any) error {
	err := get().Struct(s)
	if err == nil {
		return nil
	}
	ves, ok := errors.AsType[validator.ValidationErrors](err)
	if !ok {
		return err
	}
	out := &ValidationError{Fields: make([]FieldError, 0, len(ves))}
	mu.Lock()
	defer mu.Unlock()
	for _, fe := range ves {
		code, known := tagCodes[fe.Tag()]
		if !known {
			code = CodeInvalid
		}
		out.Fields = append(out.Fields, FieldError{Field: fieldPath(fe.Namespace()), Code: code, Tag: fe.Tag(), Param: fe.Param()})
	}
	return out
}

// Struct is Validate converted to a problem: nil when valid, a 422 problem with
// errors[] for rule failures, a 500 problem (cause kept, never serialized) for
// programmer errors.
func Struct(s any) *problem.Problem {
	err := Validate(s)
	if err == nil {
		return nil
	}
	return ToProblem(err)
}

// ToProblem maps a Validate error to its problem. Any non-ValidationError becomes
// a generic 500 so internals never reach the client.
func ToProblem(err error) *problem.Problem {
	ve, ok := IsValidationError(err)
	if !ok {
		slog.Error("validation misconfigured", "err", err)
		return problem.InternalServer("internal error").WithCause(err)
	}
	errs := make([]problem.FieldError, 0, len(ve.Fields))
	for _, f := range ve.Fields {
		errs = append(errs, problem.FieldError{Field: f.Field, Message: f.Message(), Tag: f.Code})
	}
	return problem.Validation(errs)
}

// fieldPath drops the root struct name: "Body.items[0].amount" -> "items[0].amount".
func fieldPath(ns string) string {
	if i := strings.IndexByte(ns, '.'); i >= 0 {
		return ns[i+1:]
	}
	return ns
}
