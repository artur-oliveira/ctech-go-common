package inter

import (
	"fmt"
	"strconv"
	"strings"
)

// Reais renders centavos as Inter's decimal string ("99.90").
func Reais(cents int64) string { return fmt.Sprintf("%d.%02d", cents/100, cents%100) }

// Cents parses Inter's decimal string; at most two decimals, never negative.
func Cents(s string) (int64, error) {
	whole, frac, _ := strings.Cut(strings.TrimSpace(s), ".")
	if whole == "" || len(frac) > 2 || strings.HasPrefix(whole, "-") {
		return 0, fmt.Errorf("inter: amount %q", s)
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("inter: amount %q", s)
	}
	for len(frac) < 2 {
		frac += "0"
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("inter: amount %q", s)
	}
	return w*100 + f, nil
}
