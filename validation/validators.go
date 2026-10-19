package validation

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/go-playground/validator/v10"
)

const (
	cpfLen  = 11
	cnpjLen = 14
	// MaxMoneyCents is the upper bound money_cents enforces when the tag has no
	// parameter: the largest integer a JS client (float64) holds exactly.
	MaxMoneyCents int64 = 1<<53 - 1
)

var (
	phoneRe = regexp.MustCompile(`^\d{10,11}$`) // BR landline (10) or mobile (11), DDD included
	zipRe   = regexp.MustCompile(`^\d{8}$`)

	cnpjW1 = [...]int{5, 4, 3, 2, 9, 8, 7, 6, 5, 4, 3, 2}
	cnpjW2 = [...]int{6, 5, 4, 3, 2, 9, 8, 7, 6, 5, 4, 3, 2}
)

var builtinValidators = map[string]validator.Func{
	TagCPF:        func(fl validator.FieldLevel) bool { return IsValidCPF(fl.Field().String()) },
	TagCNPJ:       func(fl validator.FieldLevel) bool { return IsValidCNPJ(fl.Field().String()) },
	TagCPFCNPJ:    func(fl validator.FieldLevel) bool { s := fl.Field().String(); return IsValidCPF(s) || IsValidCNPJ(s) },
	TagPhone:      func(fl validator.FieldLevel) bool { return IsValidPhone(fl.Field().String()) },
	TagZip:        func(fl validator.FieldLevel) bool { return IsValidZip(fl.Field().String()) },
	TagMoneyCents: moneyCents,
}

// strip removes the punctuation people type into a formatted document, and
// reports false if anything else (an emoji, a stray symbol) is present.
func strip(s, separators string) (string, bool) {
	var b strings.Builder
	for _, r := range s {
		switch {
		case strings.ContainsRune(separators, r):
		case r >= '0' && r <= '9', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
			b.WriteRune(r)
		default:
			return "", false
		}
	}
	return b.String(), true
}

func allSame(s string) bool {
	for i := 1; i < len(s); i++ {
		if s[i] != s[0] {
			return false
		}
	}
	return true
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// IsValidCPF reports whether s is a CPF with correct check digits. Formatting
// ("." "-" and spaces) is tolerated; repeated-digit sequences (which pass the
// checksum) are rejected.
func IsValidCPF(s string) bool {
	c, ok := strip(s, ".- ")
	if !ok || len(c) != cpfLen || !isDigits(c) || allSame(c) {
		return false
	}
	return cpfDigit(c, 9) == int(c[9]-'0') && cpfDigit(c, 10) == int(c[10]-'0')
}

func cpfDigit(c string, n int) int {
	sum := 0
	for i := range n {
		sum += int(c[i]-'0') * (n + 1 - i)
	}
	if d := 11 - sum%11; d < 10 {
		return d
	}
	return 0
}

// IsValidCNPJ reports whether s is a CNPJ with correct check digits, including
// the alphanumeric format (IN RFB 2229/2024): each character's value is its
// ASCII code minus 48 (so "A" is 17) and the last two characters must be
// digits. Formatting ("." "/" "-" and spaces) is tolerated.
func IsValidCNPJ(s string) bool {
	c, ok := strip(s, "./- ")
	c = strings.ToUpper(c)
	if !ok || len(c) != cnpjLen || allSame(c) || !isDigits(c[12:]) {
		return false
	}
	return cnpjDigit(c, cnpjW1[:]) == int(c[12]-'0') && cnpjDigit(c, cnpjW2[:]) == int(c[13]-'0')
}

func cnpjDigit(c string, w []int) int {
	sum := 0
	for i, wt := range w {
		sum += (int(c[i]) - '0') * wt
	}
	if r := sum % 11; r >= 2 {
		return 11 - r
	}
	return 0
}

// IsValidPhone: a Brazilian phone, DDD + 8 or 9 digits. "(", ")", "-" and spaces are tolerated.
func IsValidPhone(s string) bool {
	c, ok := strip(s, "()- ")
	return ok && phoneRe.MatchString(c)
}

// IsValidZip: a Brazilian CEP, 8 digits, optional "-".
func IsValidZip(s string) bool {
	c, ok := strip(s, "-")
	return ok && zipRe.MatchString(c)
}

// moneyCents accepts integer kinds only: positive, at most MaxMoneyCents or the
// tag parameter (`money_cents=100000000`). Floats and strings are rejected so
// money never travels as a float.
func moneyCents(fl validator.FieldLevel) bool {
	limit := MaxMoneyCents
	if p := fl.Param(); p != "" {
		v, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return false
		}
		limit = v
	}
	f := fl.Field()
	switch {
	case f.CanInt():
		return f.Int() > 0 && f.Int() <= limit
	case f.CanUint():
		return f.Uint() > 0 && f.Uint() <= uint64(limit)
	}
	return false
}
