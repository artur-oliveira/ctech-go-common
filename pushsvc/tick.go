package pushsvc

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	// tickPage is one read of a slot's reminders.
	tickPage = 100
	// tickWorkers is how many reminders are decided at once.
	tickWorkers = 8
)

// SlotReminders is what a Tick needs of ReminderStore.
type SlotReminders interface {
	ListSlot(ctx context.Context, slot, limit int, cursor map[string]types.AttributeValue) ([]Reminder, map[string]types.AttributeValue, error)
	MarkSent(ctx context.Context, sub, kind, day string) (bool, error)
}

// DeviceLister lists a person's registered devices (Store).
type DeviceLister interface {
	List(ctx context.Context, sub string) ([]Subscription, error)
}

// Decider is a product's rule: whether a due reminder goes out, and what it says. A nil job means "not today"; an error
// leaves the day open for the catch-up run.
type Decider interface {
	Decide(ctx context.Context, now time.Time, rem Reminder) (*Job, error)
}

// JobSender hands a job to the push-sender Lambda (Invoker).
type JobSender interface {
	Send(ctx context.Context, job Job) error
}

// Tick is the scheduled run of a product's reminders, every 15 minutes: it reads the reminders due in this slot (and the one
// before, to recover a skipped run), asks the product's rule whether each goes out, marks the day, and hands the push to the
// sender. See README.md for how a product wires it.
type Tick struct {
	slots   SlotReminders
	devices DeviceLister
	rules   Decider
	sender  JobSender
	onSent  func(ctx context.Context, rem Reminder)
}

// NewTick returns a Tick.
func NewTick(slots SlotReminders, devices DeviceLister, rules Decider, sender JobSender) *Tick {
	return &Tick{slots: slots, devices: devices, rules: rules, sender: sender}
}

// OnSent sets a hook run after a push was handed to the sender (Poker moves a "come play" reminder to the hour the player
// usually plays). Best effort: it cannot fail the run, and it runs on the tick's worker goroutines.
func (t *Tick) OnSent(fn func(ctx context.Context, rem Reminder)) *Tick {
	t.onSent = fn
	return t
}

type tickOutcome int

const (
	tickSkipped tickOutcome = iota
	tickSent
	tickFailed
)

// Run processes the slots of `now`. A failure of one person is logged and counted, never stops the others. A failure to read
// the slot returns an error, and so does a run where every attempt failed and none was sent (a systematic fault).
func (t *Tick) Run(ctx context.Context, now time.Time) error {
	var sent, skipped, failed atomic.Int64
	slots := SlotsToProcess(now)
	for _, slot := range slots {
		var cursor map[string]types.AttributeValue
		for {
			page, next, err := t.slots.ListSlot(ctx, slot, tickPage, cursor)
			if err != nil {
				return err
			}
			var wg sync.WaitGroup
			sem := make(chan struct{}, tickWorkers)
			for _, rem := range page {
				wg.Add(1)
				sem <- struct{}{}
				go func() {
					defer wg.Done()
					defer func() { <-sem }()
					switch t.one(ctx, now, rem) {
					case tickSent:
						sent.Add(1)
					case tickFailed:
						failed.Add(1)
					default:
						skipped.Add(1)
					}
				}()
			}
			wg.Wait()
			if next == nil {
				break
			}
			cursor = next
		}
	}
	slog.InfoContext(ctx, "push tick finished", "slots", slots, "sent", sent.Load(), "skipped", skipped.Load(), "failed", failed.Load())
	// One person's failure is logged and counted, nothing more. But a run where every attempt failed and none succeeded is a
	// systematic fault (a missing grant, schema drift): return it so it shows in the Lambda's Errors. The next run is safe
	// (the day is only marked once a push is about to go out).
	if failed.Load() > 0 && sent.Load() == 0 {
		return fmt.Errorf("pushsvc: push tick failed for %d reminder(s) and sent none", failed.Load())
	}
	return nil
}

func (t *Tick) one(ctx context.Context, now time.Time, rem Reminder) tickOutcome {
	log := slog.With("kind", rem.Kind, "slot", rem.Slot, "sub_hash", subHash(rem.Sub))
	devices, err := t.devices.List(ctx, rem.Sub)
	if err != nil {
		log.WarnContext(ctx, "push tick: devices not read", "error", err)
		return tickFailed
	}
	if len(devices) == 0 {
		return tickSkipped // nothing to send to, and the day stays open
	}
	job, err := t.rules.Decide(ctx, now, rem)
	if err != nil {
		log.WarnContext(ctx, "push tick: rule failed", "error", err)
		return tickFailed
	}
	if job == nil {
		return tickSkipped
	}
	// Mark first: a lost invoke is a push not sent, never one sent twice.
	won, err := t.slots.MarkSent(ctx, rem.Sub, rem.Kind, Today(now))
	if err != nil {
		log.WarnContext(ctx, "push tick: day not marked", "error", err)
		return tickFailed
	}
	if !won {
		return tickSkipped
	}
	if err := t.sender.Send(ctx, *job); err != nil {
		log.ErrorContext(ctx, "push tick: sender not invoked; the day stays marked", "error", err)
		return tickFailed
	}
	if t.onSent != nil {
		t.onSent(ctx, rem)
	}
	return tickSent
}
