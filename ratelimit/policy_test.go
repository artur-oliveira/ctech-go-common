package ratelimit

import (
	"context"
	"testing"
	"time"
)

func TestKey(t *testing.T) {
	tests := []struct {
		name  string
		parts []string
		want  string
	}{
		{"composite", []string{"tenant1", "user9"}, "tenant1:user9"},
		{"single", []string{"1.2.3.4"}, "1.2.3.4"},
		{"unresolved part", []string{"tenant1", ""}, ""},
		{"none", nil, ""},
	}
	for _, tt := range tests {
		if got := Key(tt.parts...); got != tt.want {
			t.Errorf("%s: Key = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestFailMode_ErrorOutcome(t *testing.T) {
	tests := []struct {
		name string
		lim  Limiter
		want Outcome
	}{
		{"default is open", Limiter{}, Allow},
		{"FailMode closed", Limiter{FailMode: FailClosed}, Unavailable},
		{"legacy FailClosed bool", Limiter{FailClosed: true}, Unavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.lim.Counter, tt.lim.Max, tt.lim.Window = failingCounter{}, 1, time.Minute
			if out, err := tt.lim.Take(context.Background(), "x"); out != tt.want || err == nil {
				t.Errorf("Take = %v, %v", out, err)
			}
			if d, err := tt.lim.Decide(context.Background(), "x"); d.Outcome != tt.want || err == nil {
				t.Errorf("Decide = %v, %v", d.Outcome, err)
			}
			if d, err := tt.lim.DecideFailures(context.Background(), "x"); d.Outcome != tt.want || err == nil {
				t.Errorf("DecideFailures = %v, %v", d.Outcome, err)
			}
		})
	}
}

func TestDecide(t *testing.T) {
	ctx := context.Background()
	lim := &Limiter{Counter: NewMemoryCounter(), Prefix: "p", Max: 2, Window: time.Minute}
	for i, want := range []struct {
		out Outcome
		rem int64
	}{{Allow, 1}, {Allow, 0}, {Deny, 0}} {
		d, err := lim.Decide(ctx, "id")
		if err != nil || d.Outcome != want.out || d.Remaining != want.rem || d.Limit != 2 {
			t.Fatalf("call %d: %+v %v", i, d, err)
		}
		if d.Reset <= 0 || d.Reset > time.Minute || d.RetryAfterSeconds() < 1 || d.RetryAfterSeconds() > 60 {
			t.Fatalf("call %d: reset %v", i, d.Reset)
		}
	}
}

func TestDecideFailures_DoesNotSpend(t *testing.T) {
	ctx := context.Background()
	lim := &Limiter{Counter: NewMemoryCounter(), Prefix: "p", Max: 2, Window: time.Minute}
	for i := 0; i < 3; i++ {
		if d, _ := lim.DecideFailures(ctx, "id"); d.Outcome != Allow {
			t.Fatalf("pre-check spent budget: %v", d.Outcome)
		}
	}
	_ = lim.RecordFailure(ctx, "id")
	_ = lim.RecordFailure(ctx, "id")
	if d, _ := lim.DecideFailures(ctx, "id"); d.Outcome != Deny || d.Remaining != 0 {
		t.Fatalf("got %+v", d)
	}
}

func TestDecision_RetryAfterSeconds(t *testing.T) {
	for _, tt := range []struct {
		reset time.Duration
		want  int
	}{{0, 1}, {1 * time.Millisecond, 1}, {1500 * time.Millisecond, 2}, {time.Minute, 60}} {
		if got := (Decision{Reset: tt.reset}).RetryAfterSeconds(); got != tt.want {
			t.Errorf("%v: got %d want %d", tt.reset, got, tt.want)
		}
	}
}

func TestLockout(t *testing.T) {
	ctx := context.Background()
	lo := &Lockout{Store: NewMemoryCounter(), Prefix: "login", Threshold: 3, Window: time.Minute, BlockFor: 10 * time.Minute}

	for i := 1; i <= 2; i++ {
		if blocked, err := lo.Fail(ctx, "a"); blocked || err != nil {
			t.Fatalf("failure %d: blocked=%v err=%v", i, blocked, err)
		}
	}
	if out, _, _ := lo.Check(ctx, "a"); out != Allow {
		t.Fatalf("before threshold: %v", out)
	}
	if blocked, _ := lo.Fail(ctx, "a"); !blocked {
		t.Fatal("3rd failure should block")
	}
	out, wait, _ := lo.Check(ctx, "a")
	if out != Deny || wait <= 0 || wait > 10*time.Minute {
		t.Fatalf("after threshold: %v %v", out, wait)
	}
	if out, _, _ := lo.Check(ctx, "b"); out != Allow {
		t.Fatalf("other identity affected: %v", out)
	}
	// A success must not lift an active block.
	_ = lo.Reset(ctx, "a")
	if out, _, _ := lo.Check(ctx, "a"); out != Deny {
		t.Fatalf("Reset lifted the block: %v", out)
	}
	_ = lo.Unblock(ctx, "a")
	if out, _, _ := lo.Check(ctx, "a"); out != Allow {
		t.Fatalf("Unblock: %v", out)
	}
}

func TestLockout_ResetClearsCount(t *testing.T) {
	ctx := context.Background()
	lo := &Lockout{Store: NewMemoryCounter(), Prefix: "l", Threshold: 2, Window: time.Minute, BlockFor: time.Minute}
	_, _ = lo.Fail(ctx, "a")
	_ = lo.Reset(ctx, "a")
	if blocked, _ := lo.Fail(ctx, "a"); blocked {
		t.Fatal("count survived Reset")
	}
}

type failingStore struct{ failingCounter }

func (failingStore) TTL(context.Context, string) (time.Duration, error) { return 0, nil }
func (failingStore) Delete(context.Context, string) error               { return nil }

func TestLockout_CheckFailMode(t *testing.T) {
	for _, tt := range []struct {
		mode FailMode
		want Outcome
	}{{FailOpen, Allow}, {FailClosed, Unavailable}} {
		lo := &Lockout{Store: failingStore{}, Threshold: 1, Window: time.Minute, BlockFor: time.Minute, FailMode: tt.mode}
		if out, _, err := lo.Check(context.Background(), "a"); out != tt.want || err == nil {
			t.Errorf("%v: got %v %v", tt.mode, out, err)
		}
	}
}

func TestIPExtractor(t *testing.T) {
	ex, err := NewIPExtractor("10.0.0.0/8", "192.168.1.1")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, remote string
		xff          []string
		want         string
	}{
		{"untrusted peer ignores header", "203.0.113.9:4000", []string{"1.1.1.1"}, "203.0.113.9"},
		{"trusted peer, single hop", "10.0.0.5:80", []string{"198.51.100.7"}, "198.51.100.7"},
		{"spoofed left side ignored", "10.0.0.5:80", []string{"6.6.6.6, 198.51.100.7, 10.0.0.9"}, "198.51.100.7"},
		{"multiple header lines", "10.0.0.5:80", []string{"6.6.6.6", "198.51.100.7"}, "198.51.100.7"},
		{"all hops trusted", "10.0.0.5:80", []string{"10.1.1.1, 192.168.1.1"}, "10.0.0.5"},
		{"no header", "10.0.0.5:80", nil, "10.0.0.5"},
		{"malformed hop", "10.0.0.5:80", []string{"198.51.100.7, garbage"}, "10.0.0.5"},
		{"bare ip remote", "10.0.0.5", []string{"198.51.100.7"}, "198.51.100.7"},
		{"ipv6 client", "[::ffff:10.0.0.5]:80", []string{"2001:db8::1"}, "2001:db8::1"},
		{"not an ip", "pipe", nil, ""},
	}
	for _, tt := range tests {
		if got := ex.ClientIP(tt.remote, tt.xff...); got != tt.want {
			t.Errorf("%s: got %q want %q", tt.name, got, tt.want)
		}
	}
	var none *IPExtractor
	if got := none.ClientIP("10.0.0.5:1", "1.1.1.1"); got != "10.0.0.5" {
		t.Errorf("nil extractor trusted header: %q", got)
	}
	if _, err := NewIPExtractor("not-an-ip"); err == nil {
		t.Error("invalid proxy accepted")
	}
}
