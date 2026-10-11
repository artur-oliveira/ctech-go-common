package byok

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestDateRoundTripsAndAddsCivilDays(t *testing.T) {
	d, err := ParseDate("2026-02-27")
	if err != nil {
		t.Fatal(err)
	}
	if got := d.AddDays(2).String(); got != "2026-03-01" {
		t.Fatalf("AddDays across February = %s, want 2026-03-01", got)
	}
	if !d.Before(d.AddDays(1)) || d.Before(d) {
		t.Fatal("Before is a strict order")
	}
	if _, err := ParseDate("2026-13-01"); err == nil {
		t.Fatal("month 13 accepted")
	}
	if !(Date{}).IsZero() {
		t.Fatal("zero Date is not IsZero")
	}
	if d.Month != time.February {
		t.Fatal("Month is a time.Month")
	}
}

func TestErrorCodesRoundTrip(t *testing.T) {
	for _, err := range []error{ErrUnauthorized, ErrCredentialsRefused, ErrScopesRefused, ErrNotFound, ErrConcluded,
		ErrRateLimited, ErrUnavailable, ErrCertificateExpired, ErrCertificateInvalid, ErrKeyMismatch,
		ErrInvalidRequest, ErrOpRefused, ErrSandboxOnly} {
		code := ErrorCode(err)
		if code == "" || code == "internal" {
			t.Fatalf("%v has no wire code", err)
		}
		if back := ErrorFromCode(code, "detail"); !errors.Is(back, err) {
			t.Fatalf("code %q came back as %v", code, back)
		}
	}
	if ErrorCode(fmt.Errorf("boom")) != "internal" {
		t.Fatal("an unknown error is internal")
	}
}
