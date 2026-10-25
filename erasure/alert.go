package erasure

import (
	"context"
	"errors"
	"time"

	"gopkg.aoctech.app/api-commons/alerts"
)

// alertTimeout bounds the SNS publish so a slow topic cannot hold the consumer.
const alertTimeout = 3 * time.Second

func alertDetached(ctx context.Context, p alerts.Publisher, a alerts.Alert) {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), alertTimeout)
	defer cancel()
	p.Alert(actx, a)
}

// AlertingPurge wraps an erasure purge so a failure also tells an operator: an
// account-deletion request that cannot finish is a legal deadline (LGPD), and
// the consumer only logs it and leaves the message for redelivery. The error is
// returned unchanged, so redelivery behaves exactly as before. An error matching
// one of expected (errors.Is) is a wait, not a failure, and does not alert (dfe's
// ErrErasureSettling). A redelivered failure alerts again; the queue's
// visibility timeout paces it. Alert job: "erasure-purge-failed".
//
// Do not combine with AlertFailures on the same consumer for StagePurge:
// AlertFailures skips that stage so a purge failure alerts once.
func AlertingPurge(purge PurgeFunc, p alerts.Publisher, expected ...error) PurgeFunc {
	return func(ctx context.Context, m Message) (Ack, error) {
		ack, err := purge(ctx, m)
		if err != nil {
			for _, e := range expected {
				if errors.Is(err, e) {
					return ack, err
				}
			}
			alertDetached(ctx, p, alerts.Alert{
				Job:     "erasure-purge-failed",
				Summary: "An account-deletion purge failed; the message stays on the queue and will be redelivered.",
				Err:     err,
				Detail:  "request_id: " + m.RequestID,
			})
		}
		return ack, err
	}
}

// AlertFailures is the stock OnFailure hook: it alerts on a message that cannot
// be decoded ("erasure-decode-failed") and on an ack that cannot be sent
// ("erasure-ack-failed"). StagePurge is skipped because AlertingPurge already
// covers it (and knows which errors are expected waits).
func AlertFailures(p alerts.Publisher) func(ctx context.Context, f Failure) {
	return func(ctx context.Context, f Failure) {
		switch f.Stage {
		case StageDecode:
			alertDetached(ctx, p, alerts.Alert{
				Job:     "erasure-decode-failed",
				Summary: "An erasure queue message could not be decoded; it stays on the queue and will end in the DLQ.",
				Err:     f.Err,
			})
		case StageAck:
			alertDetached(ctx, p, alerts.Alert{
				Job:     "erasure-ack-failed",
				Summary: "An erasure ack could not be sent to ctech-account; the message stays on the queue and will be redelivered.",
				Err:     f.Err,
				Detail:  "request_id: " + f.RequestID,
			})
		}
	}
}
