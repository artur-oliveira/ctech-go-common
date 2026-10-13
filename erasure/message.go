// Package erasure is the participant side of the CTech account-deletion saga
// (ctech-account docs/specs/2026-10-06-account-deletion-saga-protocol.md):
// the message contract, per-service lock/tombstone state, the SQS consumer and
// the ack client. Orchestration lives in ctech-account, not here.
package erasure

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Version is the message schema version this package reads and writes.
const Version = 1

type Type string

const (
	TypeLocked   Type = "user.locked"
	TypeUnlocked Type = "user.unlocked"
	TypeErase    Type = "user.erase"
)

type Scope string

const (
	ScopeAccount Scope = "account" // full account deletion
	ScopeService Scope = "service" // the user leaves one service only
)

// WorkspaceKind is the kind of a ctech-account workspace.
type WorkspaceKind string

const (
	WorkspaceKindPersonal     WorkspaceKind = "personal"
	WorkspaceKindOrganization WorkspaceKind = "organization"
)

// Workspace is a workspace erased with the user, with its kind. Participants
// that treat kinds differently (billing) need it; ids alone cannot say.
type Workspace struct {
	ID   string        `json:"id"`
	Kind WorkspaceKind `json:"kind"`
}

// Message is published by ctech-account on the user-erasure SNS topic. It never
// carries personal data beyond the opaque account sub.
type Message struct {
	Version       int      `json:"version"`
	Type          Type     `json:"type"`
	RequestID     string   `json:"request_id"`
	Sub           string   `json:"sub"`
	Scope         Scope    `json:"scope"`
	Services      []string `json:"services"`
	Organizations []string `json:"organizations,omitempty"` // single-member orgs erased with the user; account scope only
	// Workspaces is a superset of Organizations that also carries each
	// workspace's kind. Additive (schema version stays 1); account scope only.
	Workspaces []Workspace `json:"workspaces,omitempty"`
	Attempt    int         `json:"attempt"`
	IssuedAt   time.Time   `json:"issued_at"` // orders lock/unlock; SQS does not
}

var ErrInvalidMessage = errors.New("erasure: invalid message")

func (m Message) Validate() error {
	var problem string
	switch {
	case m.Version != Version:
		problem = fmt.Sprintf("unsupported version %d", m.Version)
	case m.Type != TypeLocked && m.Type != TypeUnlocked && m.Type != TypeErase:
		problem = fmt.Sprintf("unknown type %q", m.Type)
	case m.RequestID == "":
		problem = "missing request_id"
	case m.Sub == "":
		problem = "missing sub"
	case m.Scope != ScopeAccount && m.Scope != ScopeService:
		problem = fmt.Sprintf("unknown scope %q", m.Scope)
	case len(m.Services) == 0:
		problem = "no target services"
	case m.Scope == ScopeService && len(m.Organizations) > 0:
		problem = "organizations require scope account"
	case m.Scope == ScopeService && len(m.Workspaces) > 0:
		problem = "workspaces require scope account"
	case m.invalidWorkspace() != "":
		problem = m.invalidWorkspace()
	case m.IssuedAt.IsZero():
		problem = "missing issued_at"
	default:
		return nil
	}
	return fmt.Errorf("%w: %s", ErrInvalidMessage, problem)
}

func (m Message) invalidWorkspace() string {
	for _, w := range m.Workspaces {
		switch {
		case w.ID == "":
			return "workspace without id"
		case w.Kind != WorkspaceKindPersonal && w.Kind != WorkspaceKindOrganization:
			return fmt.Sprintf("workspace %q has unknown kind %q", w.ID, w.Kind)
		}
	}
	return ""
}

// WorkspaceIDs is every workspace erased with the user: Organizations and
// Workspaces together, without duplicates. Use it instead of Organizations so
// a publisher that sends only one of the two fields is still honored.
func (m Message) WorkspaceIDs() []string {
	out := slices.Clone(m.Organizations)
	for _, w := range m.Workspaces {
		if !slices.Contains(out, w.ID) {
			out = append(out, w.ID)
		}
	}
	return out
}

// KindOf is the kind of an erased workspace. An id listed only in
// Organizations (a publisher that predates Workspaces) is an organization.
// ok is false for an id that is not erased with this message.
func (m Message) KindOf(id string) (kind WorkspaceKind, ok bool) {
	for _, w := range m.Workspaces {
		if w.ID == id {
			return w.Kind, true
		}
	}
	if slices.Contains(m.Organizations, id) {
		return WorkspaceKindOrganization, true
	}
	return "", false
}

// Targets reports whether service is one of the message's recipients. The SNS
// filter policy already routes by services; this is the consumer-side check.
func (m Message) Targets(service string) bool { return slices.Contains(m.Services, service) }

func Encode(m Message) ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(m)
}

// Decode parses a queue body. It accepts both raw delivery and the SNS
// envelope, so a subscription created without RawMessageDelivery still works
// instead of sending every message to the DLQ.
func Decode(body []byte) (Message, error) {
	var env struct {
		Type    string `json:"Type"`
		Message string `json:"Message"`
	}
	if json.Unmarshal(body, &env) == nil && env.Type == "Notification" {
		body = []byte(env.Message)
	}
	var m Message
	if err := json.Unmarshal(body, &m); err != nil {
		return Message{}, fmt.Errorf("%w: %v", ErrInvalidMessage, err)
	}
	if err := m.Validate(); err != nil {
		return Message{}, err
	}
	return m, nil
}

// Blocker is one reason a user cannot be erased yet. Code is stable and
// translated by the account UI (e.g. "wallet.balance_nonzero").
type Blocker struct {
	Code      string         `json:"code"`
	Detail    map[string]any `json:"detail,omitempty"`
	ActionURL string         `json:"action_url,omitempty"`
}

// Eligibility is the body of GET /internal/erasure/eligibility/{sub}.
type Eligibility struct {
	Eligible bool      `json:"eligible"`
	Blockers []Blocker `json:"blockers"`
}

// NewEligibility is eligible exactly when there are no blockers; Blockers is
// never nil so it serializes as [].
func NewEligibility(blockers ...Blocker) Eligibility {
	return Eligibility{Eligible: len(blockers) == 0, Blockers: append([]Blocker{}, blockers...)}
}

type Result string

const (
	ResultDone    Result = "done"
	ResultBlocked Result = "blocked" // a blocker appeared; nothing was erased
)

// Ack is POSTed to ctech-account's /internal/erasure/ack after a user.erase.
type Ack struct {
	RequestID string         `json:"request_id"`
	Service   string         `json:"service"`
	Result    Result         `json:"result"`
	Counts    map[string]int `json:"counts,omitempty"` // per store: erased/anonymized/retained
	Blockers  []Blocker      `json:"blockers,omitempty"`
	At        time.Time      `json:"at"`
}
