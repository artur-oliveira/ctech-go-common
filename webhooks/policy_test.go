package webhooks

import (
	"testing"
	"time"
)

func TestBackoffGrowsAndCaps(t *testing.T) {
	if Backoff(0) != 30*time.Second || Backoff(1) != 30*time.Second {
		t.Fatalf("first attempts = %v %v", Backoff(0), Backoff(1))
	}
	if Backoff(2) != time.Minute || Backoff(3) != 2*time.Minute {
		t.Fatalf("growth = %v %v", Backoff(2), Backoff(3))
	}
	if Backoff(50) != 12*time.Hour {
		t.Fatalf("cap = %v", Backoff(50))
	}
}

func TestPolicyConstants(t *testing.T) {
	if MaxFailureRun != 12 || MaxDeliveryAttempts != 8 || DeliveryTimeout != 10*time.Second || ResponseKeep != 512 {
		t.Fatal("policy constants drifted from the spec")
	}
}
