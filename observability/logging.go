// Package observability provides the shared structured-logging contract used
// by CTech services. It deliberately has no exporter or tracing dependency:
// callers decide where slog records are shipped.
package observability

import (
	"context"
	"log/slog"
)

type requestIDContextKey struct{}

// WithRequestID attaches a safe HTTP correlation identifier to ctx.
func WithRequestID(ctx context.Context, requestID string) context.Context {
	if requestID == "" {
		return ctx
	}
	return context.WithValue(ctx, requestIDContextKey{}, requestID)
}

// RequestID returns the correlation identifier attached at an HTTP boundary.
func RequestID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	requestID, _ := ctx.Value(requestIDContextKey{}).(string)
	return requestID
}

// Error logs an operational failure, enriching it with request_id when the
// supplied context originated from an HTTP request.
func Error(ctx context.Context, message string, err error, attrs ...any) {
	slog.ErrorContext(ctx, message, arguments(ctx, err, attrs)...)
}

// Warn logs degraded or best-effort work that failed without failing its
// enclosing operation.
func Warn(ctx context.Context, message string, err error, attrs ...any) {
	slog.WarnContext(ctx, message, arguments(ctx, err, attrs)...)
}

// LogHTTPError emits the platform HTTP-error record. Client rejections are
// warnings; server failures are errors. Public response details are excluded
// by design because they may contain user-controlled values.
func LogHTTPError(ctx context.Context, status int, method, path, problemType string, err error, attrs ...any) {
	base := make([]any, 0, len(attrs)+8)
	base = append(base, "status", status, "method", method, "path", path)
	if problemType != "" {
		base = append(base, "problem_type", problemType)
	}
	base = append(base, attrs...)
	if status >= 500 {
		Error(ctx, "http request failed", err, base...)
		return
	}
	Warn(ctx, "http request rejected", err, base...)
}

func arguments(ctx context.Context, err error, attrs []any) []any {
	args := make([]any, 0, len(attrs)+4)
	if requestID := RequestID(ctx); requestID != "" {
		args = append(args, "request_id", requestID)
	}
	if err != nil {
		args = append(args, "err", err)
	}
	return append(args, attrs...)
}
