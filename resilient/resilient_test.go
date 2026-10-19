package resilient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stub is a RoundTripper whose answer the test controls.
type stub struct {
	calls  atomic.Int32
	status int
	err    error
	gate   chan struct{} // when set, a call waits for it
}

func (s *stub) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	if s.gate != nil {
		<-s.gate
	}
	if s.err != nil {
		return nil, s.err
	}
	return &http.Response{StatusCode: s.status, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func get(t *testing.T, rt http.RoundTripper, url string) (*http.Response, error) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	return rt.RoundTrip(req)
}

func newBreaker(s *stub, c *clock, changes *[]string) http.RoundTripper {
	return Wrap(s, Config{
		Failures: 3, Cooldown: 10 * time.Second, MaxCooldown: 40 * time.Second, now: c.now,
		OnChange: func(host string, from, to State) {
			if changes != nil {
				*changes = append(*changes, from.String()+">"+to.String())
			}
		},
	})
}

// A run of failures opens the circuit; from then on a call is refused at once and
// the host is not touched.
func TestCircuitOpensAfterARunOfFailuresAndStopsCalling(t *testing.T) {
	s, c := &stub{status: 503}, &clock{t: time.Unix(1000, 0)}
	rt := newBreaker(s, c, nil)
	for i := 0; i < 3; i++ {
		if _, err := get(t, rt, "https://account.test/x"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	before := s.calls.Load()
	_, err := get(t, rt, "https://account.test/x")
	var open *OpenError
	if !errors.Is(err, ErrCircuitOpen) || !errors.As(err, &open) || open.Host != "account.test" || open.RetryAfter <= 0 {
		t.Fatalf("err = %v, want an open-circuit error naming the host", err)
	}
	if s.calls.Load() != before {
		t.Fatal("an open circuit still called the host")
	}
}

// A success in between resets the run: failures must be consecutive.
func TestASuccessResetsTheRun(t *testing.T) {
	s, c := &stub{status: 503}, &clock{t: time.Unix(1000, 0)}
	rt := newBreaker(s, c, nil)
	get(t, rt, "https://h.test/")
	get(t, rt, "https://h.test/")
	s.status = 200
	get(t, rt, "https://h.test/")
	s.status = 503
	get(t, rt, "https://h.test/")
	get(t, rt, "https://h.test/")
	if _, err := get(t, rt, "https://h.test/"); errors.Is(err, ErrCircuitOpen) {
		t.Fatal("the circuit opened on 2+1 failures with a success between")
	}
}

// After the cooldown one probe goes through while the rest are refused; a good probe
// closes the circuit, a bad one reopens it for twice as long.
func TestAfterTheCooldownOneProbeDecidesAndAFailedProbeDoublesTheWait(t *testing.T) {
	s, c := &stub{status: 503}, &clock{t: time.Unix(1000, 0)}
	var changes []string
	rt := newBreaker(s, c, &changes)
	for i := 0; i < 3; i++ {
		get(t, rt, "https://h.test/")
	}
	c.add(11 * time.Second) // cooldown over

	s.gate = make(chan struct{})
	probeDone := make(chan error)
	go func() { _, err := get(t, rt, "https://h.test/"); probeDone <- err }()
	for s.calls.Load() < 4 { // the probe is in flight
		time.Sleep(time.Millisecond)
	}
	if _, err := get(t, rt, "https://h.test/"); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("a second call during the probe got %v, want it refused", err)
	}
	close(s.gate)
	<-probeDone // the probe got a 503: reopened, wait doubled to 20 s
	s.gate = nil

	c.add(15 * time.Second) // 15 < 20: still open
	if _, err := get(t, rt, "https://h.test/"); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("after a failed probe the circuit reopened for less than the doubled wait: %v", err)
	}
	c.add(6 * time.Second) // 21 s: probe again, and the host is back
	s.status = 200
	if resp, err := get(t, rt, "https://h.test/"); err != nil || resp.StatusCode != 200 {
		t.Fatalf("probe: %v", err)
	}
	if _, err := get(t, rt, "https://h.test/"); err != nil {
		t.Fatalf("the circuit did not close after a good probe: %v", err)
	}
	want := "closed>open open>half-open half-open>open open>half-open half-open>closed"
	if got := strings.Join(changes, " "); got != want {
		t.Fatalf("state changes = %q, want %q", got, want)
	}
}

// Only a host that is down counts: a 404, a 429 or a 500 is the answer of one that is
// up; the caller cancelling its own request is neither.
func TestOnlyAHostThatIsDownCounts(t *testing.T) {
	for _, status := range []int{404, 429, 500} {
		s, c := &stub{status: status}, &clock{t: time.Unix(1000, 0)}
		rt := newBreaker(s, c, nil)
		for i := 0; i < 10; i++ {
			if _, err := get(t, rt, "https://h.test/"); err != nil {
				t.Fatalf("status %d opened the circuit at call %d: %v", status, i, err)
			}
		}
	}
	s, c := &stub{err: context.Canceled}, &clock{t: time.Unix(1000, 0)}
	rt := newBreaker(s, c, nil)
	for i := 0; i < 10; i++ {
		if _, err := get(t, rt, "https://h.test/"); errors.Is(err, ErrCircuitOpen) {
			t.Fatal("the caller's own cancellation opened the circuit")
		}
	}
	s2, c2 := &stub{err: errors.New("connection refused")}, &clock{t: time.Unix(1000, 0)}
	rt2 := newBreaker(s2, c2, nil)
	for i := 0; i < 3; i++ {
		get(t, rt2, "https://h.test/")
	}
	if _, err := get(t, rt2, "https://h.test/"); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("three refused connections did not open the circuit: %v", err)
	}
}

// One host going down does not cut off another.
func TestHostsAreIndependent(t *testing.T) {
	s, c := &stub{status: 503}, &clock{t: time.Unix(1000, 0)}
	rt := newBreaker(s, c, nil)
	for i := 0; i < 3; i++ {
		get(t, rt, "https://down.test/")
	}
	s.status = 200
	if _, err := get(t, rt, "https://up.test/"); err != nil {
		t.Fatalf("another host was refused: %v", err)
	}
	if _, err := get(t, rt, "https://down.test/"); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("the failing host was not refused: %v", err)
	}
}

// A probe the caller cancels says nothing about the host: the next call probes.
func TestACancelledProbeIsNotAVerdict(t *testing.T) {
	s, c := &stub{status: 503}, &clock{t: time.Unix(1000, 0)}
	rt := newBreaker(s, c, nil)
	for i := 0; i < 3; i++ {
		get(t, rt, "https://h.test/")
	}
	c.add(11 * time.Second)
	s.err = context.Canceled
	get(t, rt, "https://h.test/") // the probe, cancelled
	s.err, s.status = nil, 200
	if _, err := get(t, rt, "https://h.test/"); err != nil {
		t.Fatalf("after a cancelled probe the next call was refused: %v", err)
	}
}

func TestTheTransportKeepsEnoughIdleConnectionsPerHost(t *testing.T) {
	tr := NewTransport()
	if tr.MaxIdleConnsPerHost < 32 || !tr.ForceAttemptHTTP2 || tr.TLSHandshakeTimeout == 0 {
		t.Fatalf("transport = %+v", tr)
	}
}
