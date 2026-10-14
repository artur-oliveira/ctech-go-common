package byok

import (
	"errors"
	"fmt"
)

var (
	ErrUnauthorized       = errors.New("byok: bearer refused")
	ErrCredentialsRefused = errors.New("byok: credentials refused")
	ErrScopesRefused      = errors.New("byok: scopes refused")
	ErrNotFound           = errors.New("byok: not found")
	ErrConcluded          = errors.New("byok: already concluded")
	ErrRateLimited        = errors.New("byok: rate limited")
	ErrUnavailable        = errors.New("byok: provider unavailable")
	ErrCertificateExpired = errors.New("byok: certificate expired")
	ErrCertificateInvalid = errors.New("byok: certificate invalid")
	ErrKeyMismatch        = errors.New("byok: key does not match certificate")
	ErrInvalidRequest     = errors.New("byok: invalid request")
	ErrOpRefused          = errors.New("byok: operation refused by the gate")
	ErrSandboxOnly        = errors.New("byok: sandbox-only operation")
	ErrPixKeyNotFound     = errors.New("byok: destination Pix key not registered")
)

var codes = []struct {
	code string
	err  error
}{
	{"unauthorized", ErrUnauthorized}, {"credentials_refused", ErrCredentialsRefused},
	{"scopes_refused", ErrScopesRefused}, {"not_found", ErrNotFound}, {"concluded", ErrConcluded},
	{"rate_limited", ErrRateLimited}, {"unavailable", ErrUnavailable},
	{"certificate_expired", ErrCertificateExpired}, {"certificate_invalid", ErrCertificateInvalid},
	{"key_mismatch", ErrKeyMismatch}, {"invalid_request", ErrInvalidRequest},
	{"op_refused", ErrOpRefused}, {"sandbox_only", ErrSandboxOnly}, {"pix_key_not_found", ErrPixKeyNotFound},
}

// ErrorCode is the wire code of err; "internal" for anything unclassified.
func ErrorCode(err error) string {
	for _, c := range codes {
		if errors.Is(err, c.err) {
			return c.code
		}
	}
	return "internal"
}

// ErrorFromCode rebuilds the sentinel on the caller's side. message never
// carries a secret (the Lambda only sends sanitized text).
func ErrorFromCode(code, message string) error {
	for _, c := range codes {
		if c.code == code {
			return fmt.Errorf("%w: %s", c.err, message)
		}
	}
	return fmt.Errorf("byok: %s", message)
}
