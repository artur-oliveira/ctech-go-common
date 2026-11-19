package pushsvc

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// 20:00 in São Paulo on 2026-11-18 is slot 80.
var tickNow = time.Date(2026, 11, 18, 23, 0, 0, 0, time.UTC)

type fakeSlots struct {
	mu     sync.Mutex
	rows   []Reminder
	marked map[string]string
	marks  int
	err    error
}

func newSlots(rs ...Reminder) *fakeSlots { return &fakeSlots{rows: rs, marked: map[string]string{}} }

func (f *fakeSlots) ListSlot(_ context.Context, slot, _ int, _ map[string]types.AttributeValue) ([]Reminder, map[string]types.AttributeValue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, nil, f.err
	}
	var out []Reminder
	for _, r := range f.rows {
		if r.Enabled && r.Slot == slot {
			out = append(out, r)
		}
	}
	return out, nil, nil
}

func (f *fakeSlots) MarkSent(_ context.Context, sub, kind, day string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.marks++
	if f.marked[sub+"/"+kind] == day {
		return false, nil
	}
	f.marked[sub+"/"+kind] = day
	return true, nil
}

type fakeDevs map[string]int

func (f fakeDevs) List(_ context.Context, sub string) ([]Subscription, error) {
	return make([]Subscription, f[sub]), nil
}

type fakeDecider struct{ fail, skip map[string]bool }

func (f fakeDecider) Decide(_ context.Context, _ time.Time, r Reminder) (*Job, error) {
	if f.fail[r.Sub] {
		return nil, errors.New("boom")
	}
	if f.skip[r.Sub] {
		return nil, nil
	}
	return &Job{EventID: r.Kind + ":" + r.Sub, Recipients: []string{r.Sub}}, nil
}

type fakeJobs struct {
	mu   sync.Mutex
	jobs []Job
	err  error
}

func (f *fakeJobs) Send(_ context.Context, j Job) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs = append(f.jobs, j)
	return f.err
}

func due(sub string) Reminder { return Reminder{Sub: sub, Kind: "streak", Enabled: true, Slot: 80} }

func TestTickSendsOncePerDayWhenRunTwice(t *testing.T) {
	jobs := &fakeJobs{}
	tk := NewTick(newSlots(due("u1")), fakeDevs{"u1": 1}, fakeDecider{}, jobs)
	for i := 0; i < 2; i++ {
		if err := tk.Run(context.Background(), tickNow); err != nil {
			t.Fatal(err)
		}
	}
	if len(jobs.jobs) != 1 {
		t.Errorf("sent %d, want 1", len(jobs.jobs))
	}
}

func TestTickProcessesThePreviousSlot(t *testing.T) {
	late := due("u1")
	late.Slot = 79
	jobs := &fakeJobs{}
	if err := NewTick(newSlots(late), fakeDevs{"u1": 1}, fakeDecider{}, jobs).Run(context.Background(), tickNow); err != nil {
		t.Fatal(err)
	}
	if len(jobs.jobs) != 1 {
		t.Errorf("a reminder of the previous slot must be recovered, sent %d", len(jobs.jobs))
	}
}

func TestTickSkipsAPersonWithoutDeviceAndLeavesTheDayOpen(t *testing.T) {
	slots, jobs := newSlots(due("u1")), &fakeJobs{}
	if err := NewTick(slots, fakeDevs{}, fakeDecider{}, jobs).Run(context.Background(), tickNow); err != nil {
		t.Fatal(err)
	}
	if len(jobs.jobs) != 0 || slots.marks != 0 {
		t.Errorf("sent=%d marks=%d, want none", len(jobs.jobs), slots.marks)
	}
}

func TestTickDoesNotMarkWhenTheRuleSaysNo(t *testing.T) {
	slots, jobs := newSlots(due("u1")), &fakeJobs{}
	if err := NewTick(slots, fakeDevs{"u1": 1}, fakeDecider{skip: map[string]bool{"u1": true}}, jobs).Run(context.Background(), tickNow); err != nil {
		t.Fatal(err)
	}
	if len(jobs.jobs) != 0 || slots.marks != 0 {
		t.Errorf("sent=%d marks=%d: a day the rule declined stays open for the catch-up", len(jobs.jobs), slots.marks)
	}
}

func TestTickMarksBeforeInvoking(t *testing.T) {
	jobs := &fakeJobs{err: errors.New("lambda unavailable")}
	tk := NewTick(newSlots(due("u1")), fakeDevs{"u1": 1}, fakeDecider{}, jobs)
	if err := tk.Run(context.Background(), tickNow); err != nil {
		t.Fatalf("a failed hand-off to the sender is logged, not a failed run: %v", err)
	}
	jobs.err = nil
	_ = tk.Run(context.Background(), tickNow)
	if len(jobs.jobs) != 1 {
		t.Errorf("sent %d: a marked day is never sent twice, even after a failed invoke", len(jobs.jobs))
	}
}

func TestTickKeepsGoingAfterOneFails(t *testing.T) {
	jobs := &fakeJobs{}
	tk := NewTick(newSlots(due("u1"), due("u2")), fakeDevs{"u1": 1, "u2": 1}, fakeDecider{fail: map[string]bool{"u1": true}}, jobs)
	if err := tk.Run(context.Background(), tickNow); err != nil {
		t.Fatal(err)
	}
	if len(jobs.jobs) != 1 || jobs.jobs[0].Recipients[0] != "u2" {
		t.Errorf("jobs = %+v", jobs.jobs)
	}
}

func TestTickFailsOnlyWhenTheSlotCannotBeRead(t *testing.T) {
	slots := newSlots(due("u1"))
	slots.err = errors.New("throttled")
	if err := NewTick(slots, fakeDevs{"u1": 1}, fakeDecider{}, &fakeJobs{}).Run(context.Background(), tickNow); err == nil {
		t.Error("an unreadable slot must fail the run so the scheduler retries")
	}
}

func TestTickCallsOnSentOnlyForWhatWasSent(t *testing.T) {
	var got []string
	tk := NewTick(newSlots(due("u1"), due("u2")), fakeDevs{"u1": 1, "u2": 1}, fakeDecider{skip: map[string]bool{"u2": true}}, &fakeJobs{}).
		OnSent(func(_ context.Context, r Reminder) { got = append(got, r.Sub) })
	_ = tk.Run(context.Background(), tickNow)
	if len(got) != 1 || got[0] != "u1" {
		t.Errorf("OnSent = %v, want only u1", got)
	}
}
