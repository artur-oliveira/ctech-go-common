package erasure

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// PurgeFunc erases/anonymizes the service's data for m.Sub (and every org in
// m.Organizations) per the data-inventory spec. It must be idempotent and
// resumable, re-check eligibility first, and return Result done or blocked.
type PurgeFunc func(ctx context.Context, m Message) (Ack, error)

type sqsAPI interface {
	ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, opts ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, in *sqs.DeleteMessageInput, opts ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
}

type Consumer struct {
	sqs      sqsAPI
	queueURL string
	service  string
	store    *Store
	purge    PurgeFunc
	acks     *AckClient
	now      func() time.Time
}

func NewConsumer(sqsClient sqsAPI, queueURL, service string, store *Store, purge PurgeFunc, acks *AckClient) *Consumer {
	return &Consumer{sqs: sqsClient, queueURL: queueURL, service: service, store: store, purge: purge, acks: acks, now: time.Now}
}

// Run long-polls until ctx is cancelled. A message is deleted only after it was
// fully handled; anything else is redelivered and, past the queue's
// maxReceiveCount, lands in the DLQ.
// One message per receive: each received message's visibility clock starts at
// receive, so a batch would let later messages reappear on another replica
// while earlier ones still purge.
// ponytail: no visibility heartbeat. Set the queue's visibility timeout to at
// least 2x the slowest purge; add a heartbeat if a purge ever outgrows it.
func (c *Consumer) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		out, err := c.sqs.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(c.queueURL),
			MaxNumberOfMessages: 1,
			WaitTimeSeconds:     20,
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.ErrorContext(ctx, "erasure: receive failed", "service", c.service, "error", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(5 * time.Second):
			}
			continue
		}
		for _, msg := range out.Messages {
			if err := c.handle(ctx, aws.ToString(msg.Body)); err != nil {
				slog.ErrorContext(ctx, "erasure: message left for redelivery",
					"service", c.service, "message_id", aws.ToString(msg.MessageId), "error", err)
				continue
			}
			if _, err := c.sqs.DeleteMessage(ctx, &sqs.DeleteMessageInput{
				QueueUrl:      aws.String(c.queueURL),
				ReceiptHandle: msg.ReceiptHandle,
			}); err != nil {
				slog.ErrorContext(ctx, "erasure: delete failed, message will be redelivered",
					"service", c.service, "message_id", aws.ToString(msg.MessageId), "error", err)
			}
		}
	}
	return nil
}

func (c *Consumer) handle(ctx context.Context, body string) error {
	m, err := Decode([]byte(body))
	if err != nil {
		return err
	}
	if !m.Targets(c.service) {
		return nil
	}
	key := SubKey(m.Sub)
	if m.Type != TypeErase {
		_, err := c.store.Apply(ctx, key, m)
		return err
	}

	// Lock first: the user.locked may have been lost, and writes must stop
	// before the purge starts. A no-op if already locked or erased.
	lock := m
	lock.Type = TypeLocked
	rec, err := c.store.Apply(ctx, key, lock)
	if err != nil {
		return err
	}
	if rec.State == StateActive {
		// Stale erase. If the user came back (Clear) after this very request
		// was purged, the work was done: ack it done (the first ack may have
		// been lost) without purging their new data. Anything else is
		// unexplained: leave it for redelivery and the DLQ, never claim done.
		if rec.RequestID != m.RequestID {
			return fmt.Errorf("erasure: erase %s is older than active record of request %s", m.RequestID, rec.RequestID)
		}
		return c.acks.Send(ctx, Ack{RequestID: m.RequestID, Service: c.service, Result: ResultDone, At: c.now().UTC()})
	}
	ack, err := c.purge(ctx, m)
	if err != nil {
		return fmt.Errorf("erasure: purge %s: %w", m.RequestID, err)
	}
	switch ack.Result {
	case ResultDone:
		for _, org := range m.Organizations {
			if _, err := c.store.Apply(ctx, OrgKey(org), m); err != nil {
				return err
			}
		}
		if _, err := c.store.Apply(ctx, key, m); err != nil {
			return err
		}
	case ResultBlocked:
		// Stays locked; ctech-account's support flow resolves and redrives.
	default:
		return fmt.Errorf("erasure: purge %s returned result %q", m.RequestID, ack.Result)
	}
	ack.RequestID, ack.Service, ack.At = m.RequestID, c.service, c.now().UTC()
	return c.acks.Send(ctx, ack)
}
