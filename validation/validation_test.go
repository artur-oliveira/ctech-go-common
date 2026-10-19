package validation

import (
	"net/http"
	"strings"
	"testing"

	"github.com/go-playground/validator/v10"

	"gopkg.aoctech.app/api-commons/problem"
)

type body struct {
	Name   string `json:"name" validate:"required,max=5"`
	CPF    string `json:"cpf" validate:"omitempty,cpf"`
	Amount int64  `json:"amount" validate:"money_cents=1000"`
	ID     string `json:"id" validate:"omitempty,uuid"`
	Items  []struct {
		Zip string `json:"zip_code" validate:"zip"`
	} `json:"items" validate:"dive"`
	Hidden string `json:"-" validate:"max=1"`
}

func TestValidateCodesAndPaths(t *testing.T) {
	b := body{Name: "toolongname", CPF: "111.111.111-11", Amount: 0, ID: "x", Hidden: "ab"}
	b.Items = append(b.Items, struct {
		Zip string `json:"zip_code" validate:"zip"`
	}{Zip: "123"})
	ve, ok := IsValidationError(Validate(b))
	if !ok {
		t.Fatal("want ValidationError")
	}
	got := map[string]string{}
	for _, f := range ve.Fields {
		got[f.Field] = f.Code
	}
	want := map[string]string{
		"name": CodeTooLong, "cpf": CodeInvalidCPF, "amount": CodeInvalidMoney,
		"id": CodeInvalidUUID, "items[0].zip_code": CodeInvalidZip, "Hidden": CodeTooLong,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %q want %q (all: %v)", k, got[k], v, got)
		}
	}
}

func TestValidateValidAndMisuse(t *testing.T) {
	if err := Validate(body{Name: "ok", Amount: 10}); err != nil {
		t.Fatalf("valid body: %v", err)
	}
	if err := Validate(42); err == nil {
		t.Fatal("non-struct must error")
	} else if _, ok := IsValidationError(err); ok {
		t.Fatal("non-struct must not be a ValidationError")
	}
	if p := ToProblem(Validate(42)); p.Status != http.StatusInternalServerError || p.Cause() == nil {
		t.Fatalf("misuse problem = %+v", p)
	}
}

func TestStructProblem(t *testing.T) {
	p := Struct(body{})
	if p == nil || p.Status != http.StatusUnprocessableEntity || p.Type != problem.TypeValidation {
		t.Fatalf("got %+v", p)
	}
	var found bool
	for _, e := range p.Errors {
		if e.Field == "name" && e.Tag == CodeRequired && e.Message == "required" {
			found = true
		}
	}
	if !found {
		t.Fatalf("name/required missing in %+v", p.Errors)
	}
	if Struct(body{Name: "a", Amount: 1}) != nil {
		t.Fatal("valid must be nil")
	}
}

func TestUnknownTagFallsBackToInvalid(t *testing.T) {
	type s struct {
		A string `json:"a" validate:"startswith=x"`
	}
	ve, _ := IsValidationError(Validate(s{A: "y"}))
	if ve.Fields[0].Code != CodeInvalid || ve.Fields[0].Tag != "startswith" || ve.Fields[0].Message() != messages[CodeInvalid] {
		t.Fatalf("got %+v", ve.Fields[0])
	}
}

func TestRegister(t *testing.T) {
	type s struct {
		C string `json:"c" validate:"cfop"`
	}
	fn := func(fl validator.FieldLevel) bool { return len(fl.Field().String()) == 4 }
	if err := Register("cfop", fn, "invalid_cfop"); err != nil {
		t.Fatal(err)
	}
	if Register("cfop", fn, "x") == nil || Register(TagCPF, fn, "x") == nil {
		t.Fatal("duplicate tag must be rejected")
	}
	ve, _ := IsValidationError(Validate(s{C: "1"}))
	if ve.Fields[0].Code != "invalid_cfop" {
		t.Fatalf("got %+v", ve.Fields[0])
	}
}

func TestBindJSON(t *testing.T) {
	type in struct {
		Name string `json:"name" validate:"required"`
	}
	cases := []struct {
		name       string
		body       string
		max        int
		wantStatus int
	}{
		{"ok", `{"name":"a"}`, 100, 0},
		{"unknown field", `{"name":"a","extra":1}`, 100, http.StatusBadRequest},
		{"malformed", `{"name":`, 100, http.StatusBadRequest},
		{"trailing data", `{"name":"a"}{"name":"b"}`, 100, http.StatusBadRequest},
		{"wrong type", `{"name":1}`, 100, http.StatusBadRequest},
		{"empty body", ``, 100, http.StatusBadRequest},
		{"too large", `{"name":"aaaaaaaaaa"}`, 5, http.StatusRequestEntityTooLarge},
		{"validation", `{"name":""}`, 100, http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var dst in
			p := BindJSONLimit([]byte(tc.body), &dst, tc.max)
			if tc.wantStatus == 0 {
				if p != nil {
					t.Fatalf("unexpected %+v", p)
				}
				return
			}
			if p == nil || p.Status != tc.wantStatus {
				t.Fatalf("got %+v want %d", p, tc.wantStatus)
			}
			// the body and the parse error must never reach the client
			if strings.Contains(p.Detail, "extra") || strings.Contains(p.Detail, "aaaa") || strings.Contains(p.Detail, "json:") {
				t.Fatalf("detail leaks input: %q", p.Detail)
			}
		})
	}
	var dst in
	if p := BindJSON([]byte(`{"name":"a"}`), &dst); p != nil || dst.Name != "a" {
		t.Fatalf("BindJSON default: %+v %+v", p, dst)
	}
}

func TestFreetext(t *testing.T) {
	cases := []struct {
		name string
		in   string
		rule FreetextRule
		want string
		err  error
	}{
		{"trims", "  hello world  ", FreetextRule{Min: 3, Max: 100}, "hello world", nil},
		{"too short", "  hi  ", FreetextRule{Min: 15, Max: 100}, "", ErrFreetextTooShort},
		{"too long", strings.Repeat("ab ", 50), FreetextRule{Min: 1, Max: 100}, "", ErrFreetextTooLong},
		{"repeat", "aaaaaaaaaaaaaaaaaaaa", FreetextRule{Min: 3, Max: 100}, "", ErrFreetextRepeated},
		{"punct run", "..........", FreetextRule{Min: 3, Max: 100}, "", ErrFreetextRepeated},
		{"no letters", "1234567890123456", FreetextRule{Min: 3, Max: 100}, "", ErrFreetextUnreadable},
		{"sentence", "O botão de login não funciona no Safari", FreetextRule{Min: 15, Max: 4000}, "O botão de login não funciona no Safari", nil},
		{"newline rejected", "Conta e login\nBcc: a@evil.com", FreetextRule{Min: 3, Max: 200}, "", ErrFreetextLineBreaks},
		{"crlf rejected", "Conta e login\r\nBcc: a@evil.com", FreetextRule{Min: 3, Max: 200}, "", ErrFreetextLineBreaks},
		{"newline opt-in", "Linha um\nLinha dois texto", FreetextRule{Min: 15, Max: 200, AllowNewlines: true}, "Linha um\nLinha dois texto", nil},
		{"runes not bytes", "çççç", FreetextRule{Min: 4, Max: 4}, "çççç", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Freetext(tc.in, tc.rule)
			if tc.err != nil {
				if !errorsIs(err, tc.err) {
					t.Fatalf("err = %v, want %v", err, tc.err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v want %q", got, err, tc.want)
			}
		})
	}
}
