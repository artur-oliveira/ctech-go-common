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

func TestAPixWithoutTxidInTheBatchDoesNotLoseTheChargePayment(t *testing.T) {
	body := `{"pix":[{"endToEndId":"E0","valor":"5.00"},{"endToEndId":"E1","txid":"ctb01JABCDEFGHJKMNPQRSTVWXYZ"}]}`
	got, err := NewProvider(NewClient()).ParseCallback(byok.ProductPix, nil, []byte(body))
	if err != nil || len(got.ChargeRefs) != 1 || got.ChargeRefs[0] != "ctb01JABCDEFGHJKMNPQRSTVWXYZ" || len(got.PaymentIDs) != 1 || got.PaymentIDs[0] != "E1" {
		t.Fatalf("%+v %v", got, err)
	}
}

// The body Inter really sent on 2026-10-15 (names and ids replaced).
const liveWebhookBody = `{"pix":[{"endToEndId":"E105735212026101513503PycbWHSfWA","txid":"ctb01M4ZX7WDMW42E4BHBM65CBHE4","valor":"1.0","chave":"0897ad8e-7c0a-4130-b178-1a825e847e7d","horario":"2026-10-15T13:50:48.514Z","infoPagador":"","componentesValor":{"original":{"valor":"1.00"},"juros":{"valor":"0.00"},"multa":{"valor":"0.00"},"abatimento":{"valor":"0.00"},"desconto":{"valor":"0.00"}},"devolucoes":[],"pagador":{"nome":"Fulano de Tal","cpfCnpj":"***137303**"}}]}`

func TestTheLiveWebhookBodyNamesThePayerWithAMaskedDocument(t *testing.T) {
	got, err := NewProvider(NewClient()).ParseCallback(byok.ProductPix, map[string]string{"x-conta-corrente": "481572732"}, []byte(liveWebhookBody))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ChargeRefs) != 1 || len(got.Payers) != 1 {
		t.Fatalf("%+v", got)
	}
	p := got.Payers[0]
	if p.ChargeRef != "ctb01M4ZX7WDMW42E4BHBM65CBHE4" || p.PaymentID != "E105735212026101513503PycbWHSfWA" || p.Name != "Fulano de Tal" || p.Document != "***137303**" {
		t.Fatalf("%+v", p)
	}
}

func TestABodyThatNamesNoPayerYieldsNone(t *testing.T) {
	got, err := NewProvider(NewClient()).ParseCallback(byok.ProductPix, nil, []byte(`{"pix":[{"endToEndId":"E1","txid":"ctb01JABCDEFGHJKMNPQRSTVWXYZ"}]}`))
	if err != nil || len(got.Payers) != 0 {
		t.Fatalf("%+v %v", got, err)
	}
}
