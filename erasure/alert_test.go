package erasure

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"gopkg.aoctech.app/api-commons/alerts"
)

type captureAlerts struct {
	mu  sync.Mutex
	got []alerts.Alert
}

func (c *captureAlerts) Alert(_ context.Context, a alerts.Alert) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, a)
}

var errSettling = errors.New("settling")

func TestAlertingPurge(t *testing.T) {
	boom := errors.New("boom")
	for _, tc := range []struct {
		name      string
		err       error
		wantAlert bool
	}{
		{"success", nil, false},
		{"expected wait is not a failure", fmt.Errorf("wrapped: %w", errSettling), false},
		{"real failure", boom, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &captureAlerts{}
			purge := AlertingPurge(func(context.Context, Message) (Ack, error) { return Ack{}, tc.err }, c, errSettling)
			_, err := purge(context.Background(), Message{RequestID: "req-1"})
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Fatalf("error must pass through unchanged, got %v", err)
			}
			if (len(c.got) == 1) != tc.wantAlert || len(c.got) > 1 {
				t.Fatalf("alerts = %+v, want alert=%v", c.got, tc.wantAlert)
			}
			if tc.wantAlert {
				a := c.got[0]
				if a.Job != "erasure-purge-failed" || a.Err != boom || a.Detail != "request_id: req-1" {
					t.Fatalf("alert content changed: %+v", a)
				}
			}
		})
	}
}

func TestOnFailure_DecodeAckPurge(t *testing.T) {
	ctx := context.Background()
	var got []Failure
	p := &purgeRecorder{ack: Ack{Result: ResultDone}}
	c, _, a := newTestConsumer(t, "dfe", p)
	c.OnFailure(func(_ context.Context, f Failure) { got = append(got, f) })

	if err := c.handle(ctx, "not json"); err == nil {
		t.Fatal("decode error must surface")
	}
	a.status.Store(http.StatusServiceUnavailable)
	if err := c.handle(ctx, body(t, msgAt(TypeErase, "r1", 0))); err == nil {
		t.Fatal("ack error must surface")
	}
	p.err = errors.New("boom")
	if err := c.handle(ctx, body(t, msgAt(TypeErase, "r2", 0))); err == nil {
		t.Fatal("purge error must surface")
	}
	want := []struct {
		s Stage
		r string
	}{{StageDecode, ""}, {StageAck, "r1"}, {StagePurge, "r2"}}
	if len(got) != len(want) {
		t.Fatalf("failures = %+v", got)
	}
	for i, w := range want {
		if got[i].Stage != w.s || got[i].RequestID != w.r || got[i].Err == nil {
			t.Fatalf("failure %d = %+v, want %+v", i, got[i], w)
		}
	}
}

func TestOnFailure_SuccessAndNilHookAreQuiet(t *testing.T) {
	ctx := context.Background()
	p := &purgeRecorder{ack: Ack{Result: ResultDone}}
	c, _, _ := newTestConsumer(t, "dfe", p)
	if err := c.handle(ctx, "not json"); err == nil { // nil hook: behaves as before
		t.Fatal("decode error must surface")
	}
	n := 0
	c.OnFailure(func(context.Context, Failure) { n++ })
	if err := c.handle(ctx, body(t, msgAt(TypeErase, "r1", 0))); err != nil || n != 0 {
		t.Fatalf("err=%v hook calls=%d", err, n)
	}
}

func TestOnFailure_PanicIsContained(t *testing.T) {
	c, _, _ := newTestConsumer(t, "dfe", &purgeRecorder{})
	c.OnFailure(func(context.Context, Failure) { panic("hook") })
	if err := c.handle(context.Background(), "not json"); err == nil {
		t.Fatal("the original error must still surface")
	}
}

func TestAlertFailures(t *testing.T) {
	c := &captureAlerts{}
	h := AlertFailures(c)
	boom := errors.New("boom")
	h(context.Background(), Failure{Stage: StageDecode, Err: boom})
	h(context.Background(), Failure{Stage: StageAck, RequestID: "r1", Err: boom})
	h(context.Background(), Failure{Stage: StagePurge, RequestID: "r1", Err: boom})
	if len(c.got) != 2 || c.got[0].Job != "erasure-decode-failed" || c.got[1].Job != "erasure-ack-failed" ||
		c.got[1].Detail != "request_id: r1" {
		t.Fatalf("alerts = %+v", c.got)
	}
}
