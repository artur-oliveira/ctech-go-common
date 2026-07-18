package problem

import (
	"net/http"
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
