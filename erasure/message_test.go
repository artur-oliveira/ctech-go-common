package erasure

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func validMessage() Message {
	return Message{
		Version:   Version,
		Type:      TypeErase,
		RequestID: "01JREQ",
		Sub:       "user-1",
		Scope:     ScopeAccount,
		Services:  []string{"wallet", "dfe"},
		Attempt:   1,
		IssuedAt:  time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC),
	}
}

func TestEncodeDecode_RoundTrip(t *testing.T) {
	in := validMessage()
	in.Organizations = []string{"org-1"}
	raw, err := Encode(in)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	out, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if out.RequestID != in.RequestID || out.Sub != in.Sub || !out.IssuedAt.Equal(in.IssuedAt) ||
		len(out.Organizations) != 1 || out.Organizations[0] != "org-1" {
		t.Fatalf("round trip mismatch: %+v", out)
	}
}

func TestDecode_SNSEnvelope(t *testing.T) {
	raw, _ := Encode(validMessage())
	env, _ := json.Marshal(map[string]string{"Type": "Notification", "MessageId": "m-1", "Message": string(raw)})
	out, err := Decode(env)
	if err != nil {
		t.Fatalf("Decode(envelope): %v", err)
	}
	if out.Sub != "user-1" || out.Type != TypeErase {
		t.Fatalf("unexpected message: %+v", out)
	}
}

func TestDecode_RejectsInvalid(t *testing.T) {
	cases := map[string]func(m *Message){
		"version":           func(m *Message) { m.Version = 2 },
		"type":              func(m *Message) { m.Type = "user.deleted" },
		"request_id":        func(m *Message) { m.RequestID = "" },
		"sub":               func(m *Message) { m.Sub = "" },
		"scope":             func(m *Message) { m.Scope = "tenant" },
		"services":          func(m *Message) { m.Services = nil },
		"issued_at":         func(m *Message) { m.IssuedAt = time.Time{} },
		"orgs with service": func(m *Message) { m.Scope = ScopeService; m.Organizations = []string{"org-1"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := validMessage()
			mutate(&m)
			raw, _ := json.Marshal(m)
			if _, err := Decode(raw); !errors.Is(err, ErrInvalidMessage) {
				t.Fatalf("Decode err = %v, want ErrInvalidMessage", err)
			}
			if _, err := Encode(m); !errors.Is(err, ErrInvalidMessage) {
				t.Fatalf("Encode err = %v, want ErrInvalidMessage", err)
			}
		})
	}
	if _, err := Decode([]byte("{not json")); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("malformed JSON err = %v, want ErrInvalidMessage", err)
	}
}

func TestTargets(t *testing.T) {
	m := validMessage()
	if !m.Targets("wallet") || m.Targets("poker") {
		t.Fatalf("Targets wrong for %v", m.Services)
	}
}

func TestNewEligibility_EmptyBlockersIsEligibleArray(t *testing.T) {
	raw, _ := json.Marshal(NewEligibility())
	if !strings.Contains(string(raw), `"eligible":true`) || !strings.Contains(string(raw), `"blockers":[]`) {
		t.Fatalf("unexpected JSON: %s", raw)
	}
	e := NewEligibility(Blocker{Code: "wallet.balance_nonzero"})
	if e.Eligible || len(e.Blockers) != 1 {
		t.Fatalf("unexpected eligibility: %+v", e)
	}
}
