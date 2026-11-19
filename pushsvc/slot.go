package pushsvc

import (
	"fmt"
	"time"
	_ "time/tzdata" // the zone must load on any host, a Lambda image included
)

// SlotsPerDay is the number of 15-minute slots a scheduled push is placed in.
const SlotsPerDay = 96

// zone is the one time zone of the family's scheduled pushes: the products are Brazilian and already count their days
// in São Paulo (Poker's streak, Billing's brcal). A per-user zone is a later change, not a hidden assumption.
var zone = mustLoad("America/Sao_Paulo")

func mustLoad(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		panic("pushsvc: " + err.Error())
	}
	return l
}

// SlotOf is the 15-minute slot (0 to 95) of an instant in São Paulo.
func SlotOf(t time.Time) int {
	t = t.In(zone)
	return (t.Hour()*60 + t.Minute()) / 15
}

// Today is the civil date (YYYY-MM-DD) of an instant in São Paulo.
func Today(t time.Time) string { return t.In(zone).Format("2006-01-02") }

// ParseHHMM turns "HH:MM" (24 hours, zero padded) into its slot; minutes round down to the slot's start.
func ParseHHMM(s string) (int, error) {
	var h, m int
	if len(s) != 5 || s[2] != ':' {
		return 0, fmt.Errorf("pushsvc: %q is not HH:MM", s)
	}
	if _, err := fmt.Sscanf(s, "%02d:%02d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("pushsvc: %q is not HH:MM", s)
	}
	return (h*60 + m) / 15, nil
}

// FormatSlot is the "HH:MM" start of a slot.
func FormatSlot(slot int) string { return fmt.Sprintf("%02d:%02d", slot*15/60, slot*15%60) }

// SlotsToProcess is the slot of `now` and the one before it, the second to recover a tick the scheduler skipped (the
// sent-day marker keeps it from sending twice). It never crosses midnight: a reminder missed at 23:45 is dropped, not
// sent as the next day's.
func SlotsToProcess(now time.Time) []int {
	slot := SlotOf(now)
	if slot == 0 {
		return []int{0}
	}
	return []int{slot, slot - 1}
}

// StartOfDay is 00:00 São Paulo of the civil day of an instant.
func StartOfDay(t time.Time) time.Time {
	t = t.In(zone)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, zone)
}

// UntilMidnight is how long is left of the São Paulo civil day, the natural TTL of a push about "today".
func UntilMidnight(t time.Time) time.Duration { return StartOfDay(t).AddDate(0, 0, 1).Sub(t) }
