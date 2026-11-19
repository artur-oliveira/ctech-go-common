package pushsvc

import (
	"slices"
	"testing"
	"time"
)

func TestParseHHMM(t *testing.T) {
	for in, want := range map[string]int{"00:00": 0, "12:07": 48, "23:59": 95, "19:00": 76} {
		got, err := ParseHHMM(in)
		if err != nil || got != want {
			t.Errorf("ParseHHMM(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"24:00", "7:5", "", "12:60", "ab:cd", "12:07:00", "-1:00"} {
		if _, err := ParseHHMM(in); err == nil {
			t.Errorf("ParseHHMM(%q) accepted", in)
		}
	}
}

func TestFormatSlotRoundTrips(t *testing.T) {
	for slot := 0; slot < SlotsPerDay; slot++ {
		if got, err := ParseHHMM(FormatSlot(slot)); err != nil || got != slot {
			t.Fatalf("slot %d -> %q -> %d, %v", slot, FormatSlot(slot), got, err)
		}
	}
}

func TestSlotOfUsesBRT(t *testing.T) {
	at := time.Date(2026, 11, 18, 22, 30, 0, 0, time.UTC) // 19:30 in São Paulo
	if got := SlotOf(at); got != 78 {
		t.Errorf("SlotOf = %d, want 78", got)
	}
	if got := Today(time.Date(2026, 11, 19, 2, 0, 0, 0, time.UTC)); got != "2026-11-18" { // 23:00 BRT still the 18th
		t.Errorf("Today = %q, want 2026-11-18", got)
	}
}

func TestSlotsToProcessDoesNotCrossMidnight(t *testing.T) {
	// 00:05 BRT = 03:05 UTC: slot 0 only, so a missed 23:45 reminder is not sent as the next day's.
	if got := SlotsToProcess(time.Date(2026, 11, 18, 3, 5, 0, 0, time.UTC)); !slices.Equal(got, []int{0}) {
		t.Errorf("got %v", got)
	}
	if got := SlotsToProcess(time.Date(2026, 11, 18, 22, 30, 0, 0, time.UTC)); !slices.Equal(got, []int{78, 77}) {
		t.Errorf("got %v", got)
	}
}
