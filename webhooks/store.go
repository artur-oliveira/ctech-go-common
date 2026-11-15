package webhooks

import (
	"context"
	"errors"
	"time"
)

// Endpoint is what the Deliverer needs to know about a destination.
type Endpoint struct {
	ID, URL, Secret string
	Active          bool
	// Internal marks an endpoint an operator provisioned rather than a customer
	// registered: it may sit on a private network (Config.TrustedClient is used
	// instead of the guarded one) and the deliveries of a disabled one are kept
	// for an hour (they can be re-enabled) instead of closed.
	Internal bool
	// Ref is opaque to the Deliverer, like Delivery.Ref: whatever the Store needs
	// to update this endpoint's health (the stored row, its counters). It comes
	// back in RecordHealth.
	Ref any
}

// Event is the thing being delivered. Body is the exact payload, rendered by
// the service, so the Deliverer stays payload-agnostic.
type Event struct {
	ID, Type string
	Body     []byte
}

// Delivery is one event's journey to one endpoint.
type Delivery struct {
	EventID, EndpointID string
	// Group orders deliveries: those of one Group are made one at a time, in
	// queue order, and groups run in parallel. Use one group per endpoint.
	Group    string
	Attempts int
	// NextAttemptAt is RFC 3339; empty or unparseable means due now.
	NextAttemptAt string
	// Ref is opaque to the Deliverer: the Store sets it when it lists a delivery
	// (the tenant, the row's keys) and gets it back on every later call, so it
	// never has to parse them out of an id.
	Ref any
}

// Due is a delivery with its event.
type Due struct {
	Delivery Delivery
	Event    Event
}

// Attempt is one try at a delivery, kept as it happened.
type Attempt struct {
	Number, StatusCode int
	DurationMs         int64
	Response, Error    string
}

var (
	// ErrEndpointGone is what Store.GetEndpoint answers for a deleted endpoint.
	ErrEndpointGone = errors.New("webhook endpoint no longer exists")
	// ErrEndpointInactive refuses a replay to a disabled endpoint.
	ErrEndpointInactive = errors.New("webhook endpoint is disabled")
)

// Store is the storage a service implements for the Deliverer. Fan-out (which
// event goes to which endpoint) is the service's, and so are the transactions
// behind these methods; the policy constants live in this package.
type Store interface {
	// DueDeliveries lists pending deliveries ordered by NextAttemptAt.
	DueDeliveries(ctx context.Context, limit int) ([]Due, error)
	GetEndpoint(ctx context.Context, d Delivery) (*Endpoint, error)
	RecordAttempt(ctx context.Context, d Delivery, a Attempt, now time.Time) error
	MarkDelivered(ctx context.Context, d Delivery, status int, now time.Time) error
	// MarkAttemptFailed counts the failure and schedules the next try with
	// Backoff, or gives up after MaxDeliveryAttempts.
	MarkAttemptFailed(ctx context.Context, d Delivery, status int, cause string, now time.Time) error
	Defer(ctx context.Context, d Delivery, until, now time.Time) error
	Close(ctx context.Context, d Delivery, cause string, now time.Time) error
	// RecordHealth counts consecutive failures and disables the endpoint at
	// MaxFailureRun.
	RecordHealth(ctx context.Context, e Endpoint, ok bool, detail string, now time.Time) error
	// Requeue makes a delivery pending and due now, keeping its attempt log.
	Requeue(ctx context.Context, d Delivery, now time.Time) error
}
