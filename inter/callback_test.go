package inter

import (
	"errors"
	"testing"

	"gopkg.aoctech.app/api-commons/byok"
)

func TestParseCallbackTakesOnlyReferences(t *testing.T) {
	body := `{"pix":[{"endToEndId":"E1","txid":"ctb01JABCDEFGHJKMNPQRSTVWXYZ","valor":"999999.00","horario":"2026-10-11T10:00:00Z"}]}`
	got, err := NewProvider(NewClient()).ParseCallback(byok.ProductPix, map[string]string{"x-conta-corrente": "1234567"}, []byte(body))
	if err != nil || got.Account != "1234567" || got.ChargeRefs[0] != "ctb01JABCDEFGHJKMNPQRSTVWXYZ" || got.PaymentIDs[0] != "E1" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := NewProvider(NewClient()).ParseCallback("boleto", nil, []byte(body)); !errors.Is(err, byok.ErrInvalidRequest) {
		t.Fatal("phase 1 parses pix only")
	}
	if _, err := NewProvider(NewClient()).ParseCallback(byok.ProductPix, nil, []byte(`{"pix":[{"txid":"bad id!"}]}`)); !errors.Is(err, byok.ErrInvalidRequest) {
		t.Fatal("a reference outside Bacen's txid rule is refused")
	}
}
