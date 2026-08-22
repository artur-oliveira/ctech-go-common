package observability

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestErrorIncludesRequestIDAndCause(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	ctx := WithRequestID(context.Background(), "req-123")
	Error(ctx, "operation failed", errors.New("database offline"), "entity_id", "entity-1")

	for _, want := range []string{"operation failed", "request_id=req-123", `err="database offline"`, "entity_id=entity-1"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("log %q does not contain %q", output.String(), want)
		}
	}
}

func TestLogHTTPErrorUsesSeverityByStatus(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	LogHTTPError(context.Background(), 422, "POST", "/items", "/problems/validation-error", nil)
	LogHTTPError(context.Background(), 503, "GET", "/items", "/problems/internal-server-error", errors.New("timeout"))

	logs := output.String()
	if !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "status=422") {
		t.Fatalf("missing warning rejection: %s", logs)
	}
	if !strings.Contains(logs, "level=ERROR") || !strings.Contains(logs, "status=503") || !strings.Contains(logs, "err=timeout") {
		t.Fatalf("missing server failure: %s", logs)
	}
}
