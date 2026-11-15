package webhooks

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Unix(1_800_000_000, 0)

type fakeStore struct {
	mu        sync.Mutex
	due       []Due
	endpoints map[string]*Endpoint
	calls     []string
	attempts  []Attempt
	health    []bool
	refs      []any
	hrefs     []any
	detail    []string
}

func (f *fakeStore) rec(s string) { f.mu.Lock(); f.calls = append(f.calls, s); f.mu.Unlock() }

func (f *fakeStore) DueDeliveries(context.Context, int) ([]Due, error) { return f.due, nil }
func (f *fakeStore) GetEndpoint(_ context.Context, d Delivery) (*Endpoint, error) {
	if e, ok := f.endpoints[d.EndpointID]; ok {
		return e, nil
	}
	return nil, ErrEndpointGone
}
func (f *fakeStore) RecordAttempt(_ context.Context, _ Delivery, a Attempt, _ time.Time) error {
	f.mu.Lock()
	f.attempts = append(f.attempts, a)
	f.mu.Unlock()
	return nil
}
func (f *fakeStore) MarkDelivered(_ context.Context, d Delivery, status int, _ time.Time) error {
	f.mu.Lock()
	f.refs = append(f.refs, d.Ref)
	f.mu.Unlock()
	f.rec("delivered:" + d.EventID)
	return nil
}
func (f *fakeStore) MarkAttemptFailed(_ context.Context, d Delivery, status int, cause string, _ time.Time) error {
	f.rec("failed:" + d.EventID + ":" + cause)
	return nil
}
func (f *fakeStore) Defer(_ context.Context, d Delivery, until, now time.Time) error {
	f.rec("defer:" + d.EventID + ":" + until.Sub(now).String())
	return nil
}
func (f *fakeStore) Close(_ context.Context, d Delivery, cause string, _ time.Time) error {
	f.rec("close:" + d.EventID + ":" + cause)
	return nil
}
func (f *fakeStore) RecordHealth(_ context.Context, e Endpoint, ok bool, detail string, _ time.Time) error {
	f.mu.Lock()
	f.hrefs = append(f.hrefs, e.Ref)
	f.health = append(f.health, ok)
	f.detail = append(f.detail, detail)
	f.mu.Unlock()
	return nil
}
func (f *fakeStore) Requeue(_ context.Context, d Delivery, _ time.Time) error {
	f.rec("requeue:" + d.EventID)
	return nil
}

func due(event, endpoint, group string) Due {
	return Due{
		Delivery: Delivery{EventID: event, EndpointID: endpoint, Group: group},
		Event:    Event{ID: event, Type: "document.authorized", Body: []byte(`{"id":"` + event + `"}`)},
	}
}

func deliverer(t *testing.T, s *fakeStore, h http.HandlerFunc) *Deliverer {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	for _, e := range s.endpoints {
		if e.URL == "" {
			e.URL = srv.URL
		}
	}
	return NewDeliverer(s, Config{HeaderPrefix: "X-Ctech-", UserAgent: "test/1", Client: srv.Client(), Workers: 2})
}

func TestDeliverSignsAndMarksDelivered(t *testing.T) {
	s := &fakeStore{due: []Due{due("e1", "w1", "g1")}, endpoints: map[string]*Endpoint{"w1": {ID: "w1", Secret: "s3cret", Active: true}}}
	d := deliverer(t, s, func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 64)
		n, _ := r.Body.Read(body)
		if err := Verify("s3cret", r.Header.Get("X-Ctech-Timestamp"), body[:n], r.Header.Get("X-Ctech-Signature"), t0, time.Minute); err != nil {
			t.Errorf("signature: %v", err)
		}
		if r.Header.Get("X-Ctech-Event-Id") != "e1" || r.Header.Get("X-Ctech-Event-Type") != "document.authorized" {
			t.Errorf("headers = %v", r.Header)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	res := d.Deliver(context.Background(), 10, t0)
	if res.Handled != 1 || len(s.calls) != 1 || s.calls[0] != "delivered:e1" {
		t.Fatalf("res=%+v calls=%v", res, s.calls)
	}
	if len(s.health) != 1 || !s.health[0] {
		t.Fatalf("health = %v", s.health)
	}
	if s.attempts[0].Number != 1 || s.attempts[0].StatusCode != 204 {
		t.Fatalf("attempt = %+v", s.attempts[0])
	}
}

func TestDeliverFailureRecordsAttemptAndHealth(t *testing.T) {
	s := &fakeStore{due: []Due{due("e1", "w1", "g1")}, endpoints: map[string]*Endpoint{"w1": {ID: "w1", Secret: "x", Active: true}}}
	d := deliverer(t, s, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "boom", 500) })
	d.Deliver(context.Background(), 10, t0)
	if len(s.calls) != 1 || s.calls[0] != "failed:e1:endpoint answered 500" {
		t.Fatalf("calls = %v", s.calls)
	}
	if s.health[0] || s.detail[0] != "HTTP 500" {
		t.Fatalf("health = %v %v", s.health, s.detail)
	}
	if s.attempts[0].Response != "boom\n" || s.attempts[0].Error == "" {
		t.Fatalf("attempt = %+v", s.attempts[0])
	}
}

func TestRedirectIsAFailure(t *testing.T) {
	s := &fakeStore{due: []Due{due("e1", "w1", "g1")}, endpoints: map[string]*Endpoint{"w1": {ID: "w1", Secret: "x", Active: true}}}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/x", 302) }))
	t.Cleanup(srv.Close)
	s.endpoints["w1"].URL = srv.URL
	c := srv.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	NewDeliverer(s, Config{Client: c}).Deliver(context.Background(), 10, t0)
	if len(s.calls) != 1 || s.calls[0] != "failed:e1:endpoint answered 302" {
		t.Fatalf("calls = %v", s.calls)
	}
}

func TestTripDefersRestOfSameGroupAndSparesOthers(t *testing.T) {
	s := &fakeStore{
		due: []Due{due("e1", "bad", "gbad"), due("e2", "bad", "gbad"), due("e3", "bad", "gbad"), due("e4", "good", "ggood")},
		endpoints: map[string]*Endpoint{
			"bad":  {ID: "bad", Secret: "x", Active: true},
			"good": {ID: "good", Secret: "x", Active: true},
		},
	}
	d := deliverer(t, s, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Ctech-Event-Id") == "e4" {
			return
		}
		http.Error(w, "no", 500)
	})
	res := d.Deliver(context.Background(), 10, t0)
	got := map[string]bool{}
	for _, c := range s.calls {
		got[c] = true
	}
	for _, want := range []string{"failed:e1:endpoint answered 500", "defer:e2:2m0s", "defer:e3:2m0s", "delivered:e4"} {
		if !got[want] {
			t.Errorf("missing %q in %v", want, s.calls)
		}
	}
	if res.Deferred != 2 {
		t.Fatalf("res = %+v", res)
	}
}

func TestInactiveEndpointDefersWhenInternalElseCloses(t *testing.T) {
	s := &fakeStore{
		due: []Due{due("e1", "ops", "g1"), due("e2", "cust", "g2")},
		endpoints: map[string]*Endpoint{
			"ops":  {ID: "ops", Secret: "x", Active: false, Internal: true},
			"cust": {ID: "cust", Secret: "x", Active: false},
		},
	}
	deliverer(t, s, func(http.ResponseWriter, *http.Request) { t.Error("request to an inactive endpoint") }).
		Deliver(context.Background(), 10, t0)
	got := map[string]bool{}
	for _, c := range s.calls {
		got[c] = true
	}
	if !got["defer:e1:1h0m0s"] || !got["close:e2:endpoint disabled"] {
		t.Fatalf("calls = %v", s.calls)
	}
}

func TestEndpointGoneIsClosedAsFailure(t *testing.T) {
	s := &fakeStore{due: []Due{due("e1", "w1", "g1")}, endpoints: map[string]*Endpoint{}}
	NewDeliverer(s, Config{}).Deliver(context.Background(), 10, t0)
	if len(s.calls) != 1 || s.calls[0] != "failed:e1:endpoint no longer exists" {
		t.Fatalf("calls = %v", s.calls)
	}
}

func TestDeliverStopsAtFirstNotDue(t *testing.T) {
	later := due("e2", "w1", "g1")
	later.Delivery.NextAttemptAt = t0.Add(time.Hour).Format(time.RFC3339Nano)
	s := &fakeStore{due: []Due{due("e1", "w1", "g1"), later, due("e3", "w1", "g1")}, endpoints: map[string]*Endpoint{"w1": {ID: "w1", Secret: "x", Active: true}}}
	d := deliverer(t, s, func(http.ResponseWriter, *http.Request) {})
	res := d.Deliver(context.Background(), 10, t0)
	if res.Handled != 1 || res.Deferred != 2 {
		t.Fatalf("res = %+v calls=%v", res, s.calls)
	}
}

func TestDeliverExceptSkipsAlreadyTried(t *testing.T) {
	s := &fakeStore{due: []Due{due("e1", "w1", "g1"), due("e2", "w1", "g1")}, endpoints: map[string]*Endpoint{"w1": {ID: "w1", Secret: "x", Active: true}}}
	d := deliverer(t, s, func(http.ResponseWriter, *http.Request) {})
	d.DeliverExcept(context.Background(), 10, t0, map[string]bool{Key(due("e1", "w1", "g1").Delivery): true})
	if len(s.calls) != 1 || s.calls[0] != "delivered:e2" {
		t.Fatalf("calls = %v", s.calls)
	}
}

func TestDeliverFreshAttemptsWithoutTheQueue(t *testing.T) {
	s := &fakeStore{endpoints: map[string]*Endpoint{"w1": {ID: "w1", Secret: "x", Active: true}}}
	d := deliverer(t, s, func(http.ResponseWriter, *http.Request) {})
	d.DeliverFresh(context.Background(), []Due{due("e1", "w1", "g1")}, t0)
	if len(s.calls) != 1 || s.calls[0] != "delivered:e1" {
		t.Fatalf("calls = %v", s.calls)
	}
}

func TestRedeliverRequeuesAndSends(t *testing.T) {
	s := &fakeStore{endpoints: map[string]*Endpoint{"w1": {ID: "w1", Secret: "x", Active: true}}}
	d := deliverer(t, s, func(http.ResponseWriter, *http.Request) {})
	if err := d.Redeliver(context.Background(), due("e1", "w1", "g1"), t0); err != nil {
		t.Fatal(err)
	}
	if len(s.calls) != 2 || s.calls[0] != "requeue:e1" || s.calls[1] != "delivered:e1" {
		t.Fatalf("calls = %v", s.calls)
	}
}

func TestRedeliverRefusesInactiveAndGone(t *testing.T) {
	s := &fakeStore{endpoints: map[string]*Endpoint{"off": {ID: "off", Secret: "x", Active: false}}}
	d := NewDeliverer(s, Config{})
	if err := d.Redeliver(context.Background(), due("e1", "off", "g"), t0); !errors.Is(err, ErrEndpointInactive) {
		t.Fatalf("inactive: %v", err)
	}
	if err := d.Redeliver(context.Background(), due("e1", "nope", "g"), t0); !errors.Is(err, ErrEndpointGone) {
		t.Fatalf("gone: %v", err)
	}
	if len(s.calls) != 0 {
		t.Fatalf("calls = %v", s.calls)
	}
}

func TestInternalEndpointUsesTheTrustedClient(t *testing.T) {
	plain := &http.Client{}
	d := NewDeliverer(&fakeStore{}, Config{TrustedClient: plain})
	if d.clientFor(&Endpoint{Internal: true}) != plain {
		t.Fatal("an internal endpoint got the guarded client")
	}
	if d.clientFor(&Endpoint{}) == plain {
		t.Fatal("a customer endpoint got the trusted client")
	}
}

func TestTransportErrorNeverCarriesTheURL(t *testing.T) {
	s := &fakeStore{due: []Due{due("e1", "w1", "g1")}, endpoints: map[string]*Endpoint{"w1": {ID: "w1", Secret: "x", Active: true, URL: "https://127.0.0.1:1/hook/TOKEN123?key=SECRETQ"}}}
	NewDeliverer(s, Config{Client: &http.Client{Timeout: time.Second}}).Deliver(context.Background(), 10, t0)
	for _, text := range []string{s.attempts[0].Error, s.calls[0], s.detail[0]} {
		if strings.Contains(text, "TOKEN123") || strings.Contains(text, "SECRETQ") || strings.Contains(text, "127.0.0.1") {
			t.Fatalf("URL leaked into %q", text)
		}
	}
}

type tenantRef struct {
	Org  string
	Live bool
}

func TestDeliveryRefReachesTheStoreUntouched(t *testing.T) {
	item := due("e1", "w1", "g1")
	item.Delivery.Ref = tenantRef{Org: "org_1", Live: true}
	s := &fakeStore{due: []Due{item}, endpoints: map[string]*Endpoint{"w1": {ID: "w1", Secret: "x", Active: true}}}
	deliverer(t, s, func(http.ResponseWriter, *http.Request) {}).Deliver(context.Background(), 10, t0)
	if len(s.refs) != 1 || s.refs[0] != (tenantRef{Org: "org_1", Live: true}) {
		t.Fatalf("refs = %v", s.refs)
	}
}

func TestEndpointRefReachesRecordHealthUntouched(t *testing.T) {
	ep := &Endpoint{ID: "w1", Secret: "x", Active: true, Ref: tenantRef{Org: "org_9"}}
	s := &fakeStore{due: []Due{due("e1", "w1", "g1")}, endpoints: map[string]*Endpoint{"w1": ep}}
	deliverer(t, s, func(http.ResponseWriter, *http.Request) {}).Deliver(context.Background(), 10, t0)
	if len(s.hrefs) != 1 || s.hrefs[0] != (tenantRef{Org: "org_9"}) {
		t.Fatalf("health refs = %v", s.hrefs)
	}
}
