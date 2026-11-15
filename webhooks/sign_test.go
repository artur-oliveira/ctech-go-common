package webhooks

import (
	"strings"
	"testing"
	"time"
)

func TestSignThenVerify(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	ts := "1800000000"
	body := []byte(`{"id":"evt_1"}`)
	sig := "v1=" + Sign("secret", ts, body)
	if err := Verify("secret", ts, body, sig, now, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRejectsStaleTimestamp(t *testing.T) {
	ts := "1800000000"
	body := []byte("x")
	sig := "v1=" + Sign("secret", ts, body)
	if err := Verify("secret", ts, body, sig, time.Unix(1_800_000_000, 0).Add(10*time.Minute), 5*time.Minute); err == nil {
		t.Fatal("accepted a signature older than the tolerance")
	}
}

func TestVerifyRejectsTamperedBodyAndWrongSecret(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	ts := "1800000000"
	sig := "v1=" + Sign("secret", ts, []byte("a"))
	if err := Verify("secret", ts, []byte("b"), sig, now, time.Minute); err == nil {
		t.Fatal("accepted a tampered body")
	}
	if err := Verify("other", ts, []byte("a"), sig, now, time.Minute); err == nil {
		t.Fatal("accepted the wrong secret")
	}
	if err := Verify("secret", ts, []byte("a"), strings.TrimPrefix(sig, "v1="), now, time.Minute); err == nil {
		t.Fatal("accepted a signature without the v1= scheme")
	}
}

func TestNewSecretIsLongUniqueAndPrefixed(t *testing.T) {
	a, b := NewSecret(), NewSecret()
	if a == b || !strings.HasPrefix(a, "whsec_") || len(a) != len("whsec_")+64 {
		t.Fatalf("secrets %q %q", a, b)
	}
}
