package webhooks

import "time"

const (
	// MaxFailureRun is how many consecutive failed deliveries disable an
	// endpoint. A run, not a total: a busy endpoint that fails 1% of the time
	// is a working endpoint.
	MaxFailureRun = 12
	// MaxDeliveryAttempts is where one event stops being retried. Eight
	// attempts on Backoff span roughly two days.
	MaxDeliveryAttempts = 8
	// DeliveryTimeout bounds one attempt.
	DeliveryTimeout = 10 * time.Second
	// ResponseKeep is how many bytes of the endpoint's answer an attempt keeps.
	ResponseKeep = 512
)

// Backoff is how long to wait before attempt n (1-based): exponential from 30
// seconds, capped at 12 hours so the tail of the schedule stays readable.
func Backoff(attempt int) time.Duration {
	const (
		base = 30 * time.Second
		cap_ = 12 * time.Hour
	)
	if attempt < 1 {
		attempt = 1
	}
	return min(base<<min(attempt-1, 12), cap_)
}
