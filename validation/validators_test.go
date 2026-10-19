package validation

import (
	"errors"
	"testing"
)

func errorsIs(err, target error) bool { return errors.Is(err, target) }

func TestIsValidCPF(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"52998224725", true},
		{"529.982.247-25", true},
		{"529 982 247 25", true},
		{"52998224724", false}, // bad check digit
		{"11111111111", false}, // repeated
		{"5299822472", false},  // short
		{"529982247250", false},
		{"5299822472a", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := IsValidCPF(tc.in); got != tc.want {
			t.Errorf("IsValidCPF(%q) = %v", tc.in, got)
		}
	}
}

func TestIsValidCNPJ(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"11222333000181", true},
		{"11.222.333/0001-81", true},
		{"12.ABC.345/01DE-35", true}, // alphanumeric, RFB's published example
		{"12.abc.345/01de-35", true},
		{"11222333000182", false},
		{"00000000000000", false},
		{"1122233300018", false},
		{"12ABC34501DEAB", false}, // check digits must be numeric
		{"", false},
	}
	for _, tc := range cases {
		if got := IsValidCNPJ(tc.in); got != tc.want {
			t.Errorf("IsValidCNPJ(%q) = %v", tc.in, got)
		}
	}
}

func TestPhoneZip(t *testing.T) {
	for in, want := range map[string]bool{
		"11987654321": true, "(11) 98765-4321": true, "1132654321": true,
		"987654321": false, "119876543210": false, "+5511987654321": false, "11abc54321": false,
	} {
		if got := IsValidPhone(in); got != want {
			t.Errorf("IsValidPhone(%q) = %v", in, got)
		}
	}
	for in, want := range map[string]bool{
		"01310100": true, "01310-100": true, "0131010": false, "013101000": false, "0131010a": false,
	} {
		if got := IsValidZip(in); got != want {
			t.Errorf("IsValidZip(%q) = %v", in, got)
		}
	}
}

func TestTagsViaValidate(t *testing.T) {
	type s struct {
		Tax   string  `json:"tax" validate:"cpfcnpj"`
		CNPJ  string  `json:"cnpj" validate:"cnpj"`
		ULID  string  `json:"ulid" validate:"ulid"`
		UUID  string  `json:"uuid" validate:"uuid"`
		Cents int64   `json:"cents" validate:"money_cents"`
		U     uint32  `json:"u" validate:"money_cents"`
		F     float64 `json:"f" validate:"money_cents"`
		Cap   int64   `json:"cap" validate:"money_cents=100"`
		Bad   int64   `json:"bad" validate:"money_cents=abc"`
	}
	ok := s{Tax: "52998224725", CNPJ: "11222333000181", ULID: "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		UUID: "123e4567-e89b-12d3-a456-426614174000", Cents: 1, U: 1, F: 1, Cap: 100, Bad: 1}
	ve, _ := IsValidationError(Validate(ok))
	got := map[string]string{}
	for _, f := range ve.Fields {
		got[f.Field] = f.Code
	}
	// only the float and the malformed-param fields may fail
	if len(got) != 2 || got["f"] != CodeInvalidMoney || got["bad"] != CodeInvalidMoney {
		t.Fatalf("got %v", got)
	}
	bad := ok
	bad.Tax, bad.CNPJ, bad.ULID, bad.UUID, bad.Cents, bad.U, bad.Cap = "1", "1", "1", "1", -5, 0, 101
	ve, _ = IsValidationError(Validate(bad))
	got = map[string]string{}
	for _, f := range ve.Fields {
		got[f.Field] = f.Code
	}
	for k, v := range map[string]string{"tax": CodeInvalidTaxID, "cnpj": CodeInvalidCNPJ, "ulid": CodeInvalidULID,
		"uuid": CodeInvalidUUID, "cents": CodeInvalidMoney, "u": CodeInvalidMoney, "cap": CodeInvalidMoney} {
		if got[k] != v {
			t.Errorf("%s: got %q want %q", k, got[k], v)
		}
	}
}
