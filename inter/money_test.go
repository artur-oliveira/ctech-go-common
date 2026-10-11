package inter

import "testing"

func TestMoneyRoundTrips(t *testing.T) {
	for cents, s := range map[int64]string{0: "0.00", 5: "0.05", 9990: "99.90", 123456789: "1234567.89"} {
		if Reais(cents) != s {
			t.Fatalf("Reais(%d) = %s", cents, Reais(cents))
		}
		if back, err := Cents(s); err != nil || back != cents {
			t.Fatalf("Cents(%s) = %d, %v", s, back, err)
		}
	}
	for _, bad := range []string{"", "1.2.3", "-1.00", "1.234", "abc"} {
		if _, err := Cents(bad); err == nil {
			t.Fatalf("Cents(%q) accepted", bad)
		}
	}
	if c, _ := Cents("10.5"); c != 1050 {
		t.Fatal("one decimal digit is tenths")
	}
}
