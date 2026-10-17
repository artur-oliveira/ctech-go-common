// Package dataexport is the participant side of the CTech personal-data export
// (ctech-account docs/specs/2026-10-17-data-export.md § 3): the message
// contract, the handler that builds a service's part and uploads it to the
// presigned URL the message carries, and the ack client. Orchestration lives in
// ctech-account, not here.
//
// The messages travel on the same SNS topic and per-service queue as the
// erasure saga (routed by the "services" attribute), so a service that adopts
// this package plugs Handler.Handle into erasure.Consumer.WithExporter instead
// of running a second consumer on the queue.
package dataexport

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Version is the message schema version this package reads and writes.
const Version = 1

// TypeExport is the only message type. Its value shares the erasure "type"
// field's namespace, which is how erasure.Consumer tells the two apart.
const TypeExport = "user.export"

// Message asks one service for its part of one person's export. It carries no
// personal data beyond the opaque account sub; the part itself goes to PutURL.
type Message struct {
	Version   int       `json:"version"`
	Type      string    `json:"type"`
	RequestID string    `json:"request_id"`
	Sub       string    `json:"sub"`
	Service   string    `json:"service"`
	PutURL    string    `json:"put_url"`    // presigned S3 PUT for {service}.zip, Content-Type application/zip
	ExpiresAt time.Time `json:"expires_at"` // PutURL stops working here; account republishes with a new one
	Attempt   int       `json:"attempt"`
	IssuedAt  time.Time `json:"issued_at"`
}

var ErrInvalidMessage = errors.New("dataexport: invalid message")

func (m Message) Validate() error {
	var problem string
	switch {
	case m.Version != Version:
		problem = fmt.Sprintf("unsupported version %d", m.Version)
	case m.Type != TypeExport:
		problem = fmt.Sprintf("unknown type %q", m.Type)
	case m.RequestID == "":
		problem = "missing request_id"
	case m.Sub == "":
		problem = "missing sub"
	case m.Service == "":
		problem = "missing service"
	case m.PutURL == "":
		problem = "missing put_url"
	case m.ExpiresAt.IsZero() || m.IssuedAt.IsZero():
		problem = "missing expires_at or issued_at"
	default:
		return nil
	}
	return fmt.Errorf("%w: %s", ErrInvalidMessage, problem)
}

func Encode(m Message) ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(m)
}

// unwrap strips the SNS envelope when the subscription is not raw-delivery.
func unwrap(body []byte) []byte {
	var env struct {
		Type    string `json:"Type"`
		Message string `json:"Message"`
	}
	if json.Unmarshal(body, &env) == nil && env.Type == "Notification" {
		return []byte(env.Message)
	}
	return body
}

// IsExport reports whether a queue body is an export message, whatever its
// validity. Anything else (an erasure message) is not this package's.
func IsExport(body []byte) bool {
	var t struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(unwrap(body), &t) == nil && t.Type == TypeExport
}

func Decode(body []byte) (Message, error) {
	var m Message
	if err := json.Unmarshal(unwrap(body), &m); err != nil {
		return Message{}, fmt.Errorf("%w: %v", ErrInvalidMessage, err)
	}
	if err := m.Validate(); err != nil {
		return Message{}, err
	}
	return m, nil
}

type Result string

const (
	ResultDone   Result = "done"
	ResultFailed Result = "failed" // the service cannot build the part; account shows it and offers a retry
)

// Ack is POSTed to ctech-account's /internal/export/ack after the upload.
type Ack struct {
	RequestID string    `json:"request_id"`
	Service   string    `json:"service"`
	Result    Result    `json:"result"`
	Size      int64     `json:"size,omitempty"`
	At        time.Time `json:"at"`
}
