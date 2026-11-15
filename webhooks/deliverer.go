package webhooks

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	// disabledRecheck is how far down the queue an internal disabled endpoint's
	// delivery moves: re-enabling it sends what it missed within the hour.
	disabledRecheck = time.Hour
	// trippedRecheck is how far down the queue the rest of an endpoint's due
	// deliveries move once an attempt to it fails in a pass: they would only fail
	// the same way, and left at the head of a queue every tenant shares they would
	// be the whole of every later batch.
	trippedRecheck = 2 * time.Minute
)

// Config tunes a Deliverer.
type Config struct {
	// HeaderPrefix names the headers: <prefix>Event-Id, Event-Type, Timestamp,
	// Signature (for example "X-Ctech-").
	HeaderPrefix string
	UserAgent    string
	// Workers is how many groups are delivered to at once (default 1).
	Workers int
	// Client delivers to customer endpoints; nil means NewGuardedClient.
	Client *http.Client
	// TrustedClient delivers to Internal endpoints; nil means a plain client.
	TrustedClient *http.Client
}

// Deliverer makes one signed HTTP attempt per (event, endpoint) and either
// finishes the delivery or schedules the next try, through a Store.
type Deliverer struct {
	store   Store
	cfg     Config
	client  *http.Client
	trusted *http.Client
}

// NewDeliverer builds a Deliverer.
func NewDeliverer(store Store, cfg Config) *Deliverer {
	d := &Deliverer{store: store, cfg: cfg, client: cfg.Client, trusted: cfg.TrustedClient}
	if d.client == nil {
		d.client = NewGuardedClient(DeliveryTimeout)
	}
	if d.trusted == nil {
		d.trusted = &http.Client{Timeout: DeliveryTimeout}
	}
	d.cfg.Workers = max(cfg.Workers, 1)
	return d
}

func (d *Deliverer) clientFor(e *Endpoint) *http.Client {
	if e.Internal {
		return d.trusted
	}
	return d.client
}

// Result is what one run did.
type Result struct {
	Examined, Handled, Deferred, Failed int
	Errors                              []string
}

func (r *Result) fail(format string, args ...any) {
	r.Failed++
	r.Errors = append(r.Errors, fmt.Sprintf(format, args...))
}

func (r *Result) add(o Result) {
	r.Examined += o.Examined
	r.Handled += o.Handled
	r.Deferred += o.Deferred
	r.Failed += o.Failed
	r.Errors = append(r.Errors, o.Errors...)
}

// Key identifies a delivery, for DeliverExcept.
func Key(d Delivery) string { return d.EventID + "#" + d.EndpointID }

// Deliver attempts the deliveries that are due. The queue is ordered by due
// time, so it stops at the first row that is not due yet.
func (d *Deliverer) Deliver(ctx context.Context, limit int, now time.Time) Result {
	return d.DeliverExcept(ctx, limit, now, nil)
}

// DeliverExcept is Deliver skipping the deliveries (by Key) this run already tried.
func (d *Deliverer) DeliverExcept(ctx context.Context, limit int, now time.Time, skip map[string]bool) Result {
	var res Result
	due, err := d.store.DueDeliveries(ctx, limit)
	if err != nil {
		res.fail("reading the delivery queue: %v", err)
		return res
	}
	var todo []Due
	for i := range due {
		if !isDue(due[i].Delivery.NextAttemptAt, now) {
			res.Deferred = len(due) - i
			break
		}
		if skip[Key(due[i].Delivery)] {
			continue
		}
		todo = append(todo, due[i])
	}
	d.attemptAll(ctx, todo, now, &res)
	return res
}

// DeliverFresh attempts deliveries a fan-out just created, without going
// through the queue (an index shows a row a moment after it is written).
func (d *Deliverer) DeliverFresh(ctx context.Context, items []Due, now time.Time) Result {
	var res Result
	d.attemptAll(ctx, items, now, &res)
	return res
}

// Redeliver replays one delivery: it is requeued and attempted at once. A
// deleted or disabled endpoint is refused and nothing is sent.
func (d *Deliverer) Redeliver(ctx context.Context, due Due, now time.Time) error {
	e, err := d.store.GetEndpoint(ctx, due.Delivery)
	if err != nil {
		return err
	}
	if !e.Active {
		return ErrEndpointInactive
	}
	if err := d.store.Requeue(ctx, due.Delivery, now); err != nil {
		return err
	}
	var res Result
	d.attempt(ctx, due, now, &res)
	if res.Failed > 0 {
		return fmt.Errorf("replay: %s", res.Errors[0])
	}
	return nil
}

// attemptAll attempts items group by group: one group's deliveries are made in
// order by one worker, and up to Workers groups run at once. When an attempt
// fails, the rest of its group's due deliveries are moved down the queue
// instead of being tried: a dead endpoint then costs one timeout per pass.
func (d *Deliverer) attemptAll(ctx context.Context, items []Due, now time.Time, res *Result) {
	var order []string
	groups := map[string][]Due{}
	for _, it := range items {
		if _, ok := groups[it.Delivery.Group]; !ok {
			order = append(order, it.Delivery.Group)
		}
		groups[it.Delivery.Group] = append(groups[it.Delivery.Group], it)
	}

	var mu sync.Mutex
	work := make(chan []Due)
	var wg sync.WaitGroup
	for w := 0; w < d.cfg.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for g := range work {
				var local Result
				for i, it := range g {
					local.Examined++
					if !d.attempt(ctx, it, now, &local) {
						continue
					}
					for _, rest := range g[i+1:] {
						if err := d.store.Defer(ctx, rest.Delivery, now.Add(trippedRecheck), now); err != nil {
							local.fail("moving delivery %s of a failing endpoint: %v", rest.Delivery.EventID, err)
							continue
						}
						local.Deferred++
					}
					break
				}
				mu.Lock()
				res.add(local)
				mu.Unlock()
			}
		}()
	}
	for _, k := range order {
		work <- groups[k]
	}
	close(work)
	wg.Wait()
}

// attempt makes one try at one delivery and records it: the attempt, the
// delivery's new state and the endpoint's health. It reports whether the
// endpoint failed.
func (d *Deliverer) attempt(ctx context.Context, item Due, now time.Time, res *Result) (endpointFailed bool) {
	del := item.Delivery
	endpoint, err := d.store.GetEndpoint(ctx, del)
	if err != nil {
		// Deleted under a queued delivery: not retryable, and not worth an
		// alarm. There is nowhere to send it.
		if markErr := d.store.MarkAttemptFailed(ctx, del, 0, "endpoint no longer exists", now); markErr != nil {
			res.fail("closing orphaned delivery %s: %v", del.EventID, markErr)
		}
		return false
	}
	// A disabled endpoint's rows must leave the head of the queue, which every
	// tenant shares. An internal endpoint (an operator may re-enable it) keeps
	// what it missed, an hour further down; a customer endpoint closes them.
	if !endpoint.Active {
		var markErr error
		if endpoint.Internal {
			markErr = d.store.Defer(ctx, del, now.Add(disabledRecheck), now)
		} else {
			markErr = d.store.Close(ctx, del, "endpoint disabled", now)
		}
		if markErr != nil {
			res.fail("moving delivery %s of a disabled endpoint: %v", del.EventID, markErr)
		}
		res.Deferred++
		return false
	}

	began := time.Now()
	status, answer, err := d.post(ctx, endpoint, item.Event, now)
	attempt := Attempt{Number: del.Attempts + 1, StatusCode: status, DurationMs: time.Since(began).Milliseconds(), Response: answer}
	if err != nil {
		attempt.Error = err.Error()
	}
	// Kept before the delivery moves on; a failure to keep it is logged and
	// never stops the delivery: the history is evidence, not the work.
	if recErr := d.store.RecordAttempt(ctx, del, attempt, now); recErr != nil {
		slog.Warn("webhook attempt not recorded", "event", del.EventID, "endpoint", endpoint.ID, "error", recErr)
	}
	if err == nil {
		if mErr := d.store.MarkDelivered(ctx, del, status, now); mErr != nil {
			res.fail("recording delivery of %s: %v", del.EventID, mErr)
			return false
		}
		res.Handled++
	} else {
		endpointFailed = true
		if mErr := d.store.MarkAttemptFailed(ctx, del, status, err.Error(), now); mErr != nil {
			res.fail("recording failed attempt for %s: %v", del.EventID, mErr)
			return false
		}
		slog.Warn("webhook delivery failed", "event", del.EventID, "endpoint", endpoint.ID, "attempt", attempt.Number, "status", status, "error", err)
	}
	if hErr := d.store.RecordHealth(ctx, *endpoint, err == nil, failureDetail(status, err), now); hErr != nil {
		res.fail("recording endpoint health for %s: %v", endpoint.ID, hErr)
	}
	return endpointFailed
}

// post makes one signed attempt, returning the status code alongside the error
// so a failure can record what the endpoint actually said.
func (d *Deliverer) post(ctx context.Context, e *Endpoint, ev Event, now time.Time) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.URL, bytes.NewReader(ev.Body))
	if err != nil {
		return 0, "", fmt.Errorf("building request: %w", err)
	}
	timestamp := strconv.FormatInt(now.Unix(), 10)
	p := d.cfg.HeaderPrefix
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", d.cfg.UserAgent)
	// The event id is a header as well as a payload field so a consumer can
	// deduplicate before parsing. Deliveries are at-least-once: a response that
	// times out after the endpoint committed looks like one that never arrived.
	req.Header.Set(p+"Event-Id", ev.ID)
	req.Header.Set(p+"Event-Type", ev.Type)
	req.Header.Set(p+"Timestamp", timestamp)
	req.Header.Set(p+"Signature", SignatureScheme+Sign(e.Secret, timestamp, ev.Body))

	resp, err := d.clientFor(e).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	// Drained so the connection can be reused; the first bytes are kept with the
	// attempt (a consumer's 401 and a gateway's HTML page are different problems).
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return resp.StatusCode, "", fmt.Errorf("draining endpoint response: %w", err)
	}
	snippet := string(answer)
	if len(snippet) > ResponseKeep {
		snippet = snippet[:ResponseKeep]
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, snippet, fmt.Errorf("endpoint answered %d", resp.StatusCode)
	}
	return resp.StatusCode, snippet, nil
}

func isDue(at string, now time.Time) bool {
	if at == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		// A corrupt due time held back forever would hide the problem;
		// attempting it surfaces it where it can be seen.
		return true
	}
	return !t.After(now)
}

// failureDetail is the status the endpoint answered with, or the transport
// error when it never answered.
func failureDetail(status int, err error) string {
	switch {
	case err == nil:
		return ""
	case status > 0:
		return fmt.Sprintf("HTTP %d", status)
	}
	return err.Error()
}
