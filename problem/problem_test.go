package problem

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestNew(t *testing.T) {
	p := New(http.StatusConflict, TypeConflict, "Conflict", "duplicate entry")
	if p.Status != http.StatusConflict {
		t.Errorf("Status = %d, want %d", p.Status, http.StatusConflict)
	}
	if p.Type != TypeConflict {
		t.Errorf("Type = %q, want %q", p.Type, TypeConflict)
	}
	if p.Title != "Conflict" {
		t.Errorf("Title = %q, want %q", p.Title, "Conflict")
	}
	if p.Detail != "duplicate entry" {
		t.Errorf("Detail = %q, want %q", p.Detail, "duplicate entry")
	}
}

func TestCauseIsAvailableButNeverSerialized(t *testing.T) {
	p := InternalServer("safe public detail").WithCause(errors.New("database password leaked"))
	if got := p.Cause(); got == nil || got.Error() != "database password leaked" {
		t.Fatalf("Cause() = %v", got)
	}
	payload, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "database password leaked") {
		t.Fatalf("private cause serialized: %s", payload)
	}
}

func TestBadRequest(t *testing.T) {
	p := BadRequest("bad input")
	if p.Status != http.StatusBadRequest {
		t.Errorf("Status = %d, want %d", p.Status, http.StatusBadRequest)
	}
	if p.Type != TypeBadRequest {
		t.Errorf("Type = %q, want %q", p.Type, TypeBadRequest)
	}
}

func TestValidationCarriesFieldErrors(t *testing.T) {
	p := Validation([]FieldError{
		{Field: "person.cpf", Message: "invalid CPF", Tag: "cpf"},
	})
	if p.Status != http.StatusUnprocessableEntity {
		t.Errorf("Status = %d, want %d", p.Status, http.StatusUnprocessableEntity)
	}
	if len(p.Errors) != 1 || p.Errors[0].Field != "person.cpf" {
		t.Errorf("Errors = %+v, want one FieldError for person.cpf", p.Errors)
	}
}

func TestNotFound(t *testing.T) {
	p := NotFound("organization not found")
	if p.Status != http.StatusNotFound {
		t.Errorf("Status = %d, want %d", p.Status, http.StatusNotFound)
	}
}

func TestInternalServer(t *testing.T) {
	p := InternalServer("unexpected failure")
	if p.Status != http.StatusInternalServerError {
		t.Errorf("Status = %d, want %d", p.Status, http.StatusInternalServerError)
	}
}

func TestTooManyRequestsCarriesRetryGuidance(t *testing.T) {
	p := TooManyRequestsAfter("slow down", 30)
	if p.NextAction != NextActionRetry || p.RetryAfterSeconds != 30 {
		t.Fatalf("next_action=%q retry_after=%d", p.NextAction, p.RetryAfterSeconds)
	}
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), `"next_action":"retry"`) || !strings.Contains(string(body), `"retry_after_seconds":30`) {
		t.Fatalf("body=%s", body)
	}
}

// An unknown window must not become a fabricated one: clients back off on
// whatever number is there, so 0 stays omitted entirely.
func TestUnknownRetryWindowIsOmitted(t *testing.T) {
	body, err := json.Marshal(TooManyRequests("slow down"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "retry_after_seconds") {
		t.Fatalf("body=%s", body)
	}
	if !strings.Contains(string(body), `"next_action":"retry"`) {
		t.Fatalf("body=%s", body)
	}
}

// Every other problem stays exactly as it was — the extension members are
// omitted unless a service opts in.
func TestProblemsWithoutGuidanceOmitTheFields(t *testing.T) {
	body, err := json.Marshal(BadRequest("nope"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "next_action") || strings.Contains(string(body), "retry_after_seconds") {
		t.Fatalf("body=%s", body)
	}
}
