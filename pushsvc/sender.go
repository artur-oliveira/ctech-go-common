package pushsvc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"gopkg.aoctech.app/api-commons/alerts"
	"gopkg.aoctech.app/api-commons/webpush"
)

// sendWorkers is how many devices are sent to at once.
const sendWorkers = 8

// ErrNotConfigured means the operator has not written the VAPID parameters (yet). It is how "push is off" looks.
var ErrNotConfigured = errors.New("pushsvc: VAPID parameters are not set")

// ParameterReader is the slice of the SSM client LoadSender uses.
type ParameterReader interface {
	GetParameter(ctx context.Context, in *ssm.GetParameterInput, optFns ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
}

// Paths are the SSM parameters of one product's VAPID setup (`/ctech-{service}/{env}/push/...`).
type Paths struct{ Private, Previous, Public, Subject string }

// Pusher is the slice of webpush.Sender the handler uses.
type Pusher interface {
	Send(ctx context.Context, sub webpush.Subscription, kid string, m webpush.Message) (webpush.Result, error)
}

// DeviceStore is what the handler needs of storage; *Store implements it.
type DeviceStore interface {
	List(ctx context.Context, sub string) ([]Subscription, error)
	Delete(ctx context.Context, sub, id string) error
}

// Handler sends a Job to every device of every recipient. It decides nothing about content and holds no state: a device
// the push service says is gone (404, 410, or a key mismatch) is deleted from the table.
type Handler struct {
	store  DeviceStore
	sender Pusher
	alerts alerts.Publisher
	now    func() time.Time
}

// NewHandler returns a Handler. sender may be nil when it is loaded later (see Lambda).
func NewHandler(store DeviceStore, sender Pusher, pub alerts.Publisher) *Handler {
	return &Handler{store: store, sender: sender, alerts: pub, now: time.Now}
}

func readParam(ctx context.Context, c ParameterReader, path string) (value string, missing bool, err error) {
	if path == "" {
		return "", true, nil
	}
	out, err := c.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(path), WithDecryption: aws.Bool(true)})
	var notFound *ssmtypes.ParameterNotFound
	if errors.As(err, &notFound) {
		return "", true, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading %s: %w", path, err)
	}
	return aws.ToString(out.Parameter.Value), false, nil
}

// LoadSender reads the VAPID keys and builds the sender. The public parameter must equal the key derived from the
// private one: a mismatch would make every subscription (made with the public key) fail with 401 at the push service.
// It returns ErrNotConfigured when the private key is not there.
func LoadSender(ctx context.Context, c ParameterReader, p Paths) (Pusher, error) {
	privatePEM, missing, err := readParam(ctx, c, p.Private)
	if err != nil {
		return nil, err
	}
	if missing {
		return nil, ErrNotConfigured
	}
	key, err := webpush.ParseKey(privatePEM)
	if err != nil {
		return nil, fmt.Errorf("pushsvc: the VAPID private key does not parse: %w", err)
	}
	publicKey, missing, err := readParam(ctx, c, p.Public)
	if err != nil {
		return nil, err
	}
	if missing || publicKey != key.PublicBase64URL() {
		return nil, errors.New("pushsvc: the VAPID public parameter is missing or does not match the private key")
	}
	keys := []webpush.Key{key}
	if previousPEM, missing, err := readParam(ctx, c, p.Previous); err != nil {
		return nil, err
	} else if !missing {
		previous, err := webpush.ParseKey(previousPEM)
		if err != nil {
			return nil, fmt.Errorf("pushsvc: the previous VAPID private key does not parse: %w", err)
		}
		keys = append(keys, previous)
	}
	subject, missing, err := readParam(ctx, c, p.Subject)
	if err != nil {
		return nil, err
	}
	if missing {
		return nil, errors.New("pushsvc: the VAPID subject (app url) parameter is missing")
	}
	return webpush.NewSender(webpush.NewClient(), subject, keys...), nil
}

// Lambda adapts h to a Lambda entry point (`lambda.Start(pushsvc.Lambda(h, ssm, paths))`). The keys are read on the first
// invocation, not at start: a function deployed before the operator wrote the parameters stays quiet instead of failing
// its init. A transient SSM failure is retried on the next invocation instead of poisoning the container.
func Lambda(h *Handler, c ParameterReader, p Paths) func(context.Context, Job) error {
	var mu sync.Mutex
	return func(ctx context.Context, job Job) error {
		mu.Lock()
		if h.sender == nil {
			s, err := LoadSender(ctx, c, p)
			if errors.Is(err, ErrNotConfigured) {
				mu.Unlock()
				slog.WarnContext(ctx, "push: VAPID parameters are not set; nothing sent")
				return nil
			}
			if err != nil {
				mu.Unlock()
				return err
			}
			h.sender = s
		}
		mu.Unlock()
		return h.Handle(ctx, job)
	}
}

// Handle sends the job to every device of every recipient. A device that refuses is deleted or logged; only a failure of
// this function's own work (the table unreadable or unwritable) returns an error, so Lambda retries the whole job.
func (h *Handler) Handle(ctx context.Context, job Job) error {
	payload, err := BuildPayload(job)
	if err != nil {
		slog.ErrorContext(ctx, "push: job refused", "event_id", job.EventID, "error", err)
		return nil // a retry would be refused the same way
	}
	msg := webpush.Message{
		Payload: payload, TTL: time.Duration(job.TTLSeconds) * time.Second, Urgency: webpush.Urgency(job.Urgency),
		Topic: job.Topic, DedupeKey: job.EventID,
	}
	var sent, gone, failed, workErrors atomic.Int64
	var vapidRejected, tooLarge atomic.Bool
	var wg sync.WaitGroup
	sem := make(chan struct{}, sendWorkers)
	for _, recipient := range job.Recipients {
		subs, err := h.store.List(ctx, recipient)
		if err != nil {
			slog.ErrorContext(ctx, "push: devices not read", "event_id", job.EventID, "sub_hash", subHash(recipient), "error", err)
			workErrors.Add(1)
			continue
		}
		for _, d := range subs {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				switch h.sendOne(ctx, job, recipient, d, msg) {
				case webpush.Delivered:
					sent.Add(1)
				case webpush.Gone:
					gone.Add(1)
				case webpush.Unauthorized:
					gone.Add(1)
					vapidRejected.Store(true)
				case webpush.PayloadTooLarge:
					failed.Add(1)
					tooLarge.Store(true)
				case 0: // the device was not reachable, or its row could not be deleted
					workErrors.Add(1)
				default:
					failed.Add(1)
				}
			}()
		}
	}
	wg.Wait()
	slog.InfoContext(ctx, "push send finished", "event_id", job.EventID, "type", job.Type, "recipients", len(job.Recipients),
		"delivered", sent.Load(), "gone", gone.Load(), "failed", failed.Load(), "work_errors", workErrors.Load())
	if vapidRejected.Load() {
		h.alerts.Alert(ctx, alerts.Alert{Job: "push-vapid-rejected", Summary: "a push service rejected the VAPID key of a subscription", Detail: "event " + job.EventID})
	}
	if tooLarge.Load() {
		h.alerts.Alert(ctx, alerts.Alert{Job: "push-payload-too-large", Summary: "a push service refused a payload as too large", Detail: "event " + job.EventID})
	}
	if n := workErrors.Load(); n > 0 {
		h.alerts.Alert(ctx, alerts.Alert{Job: "push-sender-failed", Summary: "the push sender could not complete its work", Detail: fmt.Sprintf("event %s, %d failures", job.EventID, n)})
		return fmt.Errorf("pushsvc: %d job-level failures", n)
	}
	return nil
}

// sendOne sends to one device and acts on the answer: Gone and Unauthorized delete the row. It returns the outcome, or 0
// when the job's own work failed (a device that could not be sent to at all, or deleted).
func (h *Handler) sendOne(ctx context.Context, job Job, recipient string, d Subscription, msg webpush.Message) webpush.Outcome {
	started := h.now()
	sub, err := webpush.ParseSubscription(d.Endpoint, d.P256dh, d.Auth)
	if err != nil {
		// A stored row that no longer parses can never be sent to: drop it.
		slog.WarnContext(ctx, "push: stored subscription is invalid; deleting", "event_id", job.EventID, "sub_hash", subHash(recipient), "device", d.ID)
		return h.drop(ctx, recipient, d.ID, webpush.Gone)
	}
	res, err := h.sender.Send(ctx, sub, d.VapidKID, msg)
	if errors.Is(err, webpush.ErrUnknownKey) {
		return h.drop(ctx, recipient, d.ID, webpush.Unauthorized) // its key was retired: the client resubscribes on its next load
	}
	attrs := []any{
		"event_id", job.EventID, "sub_hash", subHash(recipient), "push_host", hostOf(d.Endpoint),
		"status", res.Status, "outcome", res.Outcome.String(), "duration_ms", h.now().Sub(started).Milliseconds(),
	}
	if err != nil {
		slog.WarnContext(ctx, "push send", append(attrs, "error", err.Error())...) // err carries the host and cause only
		if res.Outcome == 0 {
			return webpush.Transient
		}
	} else {
		slog.InfoContext(ctx, "push send", attrs...)
	}
	switch res.Outcome {
	case webpush.Gone, webpush.Unauthorized:
		return h.drop(ctx, recipient, d.ID, res.Outcome)
	default:
		return res.Outcome
	}
}

func (h *Handler) drop(ctx context.Context, recipient, id string, outcome webpush.Outcome) webpush.Outcome {
	if err := h.store.Delete(ctx, recipient, id); err != nil && !errors.Is(err, ErrNotFound) {
		slog.ErrorContext(ctx, "push: subscription not deleted", "sub_hash", subHash(recipient), "device", id, "error", err)
		return 0
	}
	return outcome
}

// subHash is a short, one-way tag of a person for the logs: enough to follow one person's events, not who it is.
func subHash(sub string) string {
	sum := sha256.Sum256([]byte(sub))
	return hex.EncodeToString(sum[:4])
}

// hostOf is the push service's host, the only part of an endpoint a log may carry.
func hostOf(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
