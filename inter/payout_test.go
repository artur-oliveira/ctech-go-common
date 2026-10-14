package inter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"gopkg.aoctech.app/api-commons/byok"
)

const payoutRef = "8bbdede4-35db-4ec9-b652-e176841e62c8"

func payoutProvider(t *testing.T, h http.HandlerFunc) (*Provider, byok.Credentials) {
	t.Helper()
	p := newPKI(t, time.Now().AddDate(1, 0, 0), h)
	prov := NewProvider(NewClient(WithBaseURL(p.server.URL), WithRootCAs(p.pool)))
	return prov, byok.Credentials{Values: map[string]string{"client_id": "c", "client_secret": "s",
		"cert_pem": string(p.clientCert), "key_pem": string(p.clientKey)}, Account: "1234567"}
}

func TestPayoutSendsAKeyPaymentWithTheCallersUUID(t *testing.T) {
	var path, idem, account, method string
	var body map[string]any
	prov, cr := payoutProvider(t, func(w http.ResponseWriter, r *http.Request) {
		method, path, idem, account = r.Method, r.URL.Path, r.Header.Get("x-id-idempotente"), r.Header.Get("x-conta-corrente")
		body = decodeBody(t, r)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"tipoRetorno":"APROVACAO","codigoSolicitacao":"c42f0787-02cb-4b31-827e-459ec9d7ece1","dataPagamento":"2026-10-14","dataOperacao":"2026-10-14"}`))
	})
	got, err := prov.Payout(context.Background(), cr, byok.Token{Value: "t"}, byok.PayoutRequest{Ref: payoutRef, Amount: 1050, PixKey: "12345678909", Message: "Saque"})
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || path != "/banking/v2/pix" || idem != payoutRef || account != "1234567" {
		t.Fatalf("%s %s idem=%q account=%q", method, path, idem, account)
	}
	dest, _ := body["destinatario"].(map[string]any)
	if body["valor"] != 10.5 || body["descricao"] != "Saque" || dest["tipo"] != "CHAVE" || dest["chave"] != "12345678909" {
		t.Fatalf("body %v", body)
	}
	if got.Ref != payoutRef || got.ProviderRef != "c42f0787-02cb-4b31-827e-459ec9d7ece1" || got.Status != byok.PayoutProcessing {
		t.Fatalf("a payment just accepted is processing until the lookup says otherwise: %+v", got)
	}
}

func TestPayoutRefusesWhatInterWouldRefuseBeforeCallingIt(t *testing.T) {
	called := false
	prov, cr := payoutProvider(t, func(w http.ResponseWriter, _ *http.Request) { called = true })
	for name, in := range map[string]byok.PayoutRequest{
		"ref is not a uuid": {Ref: "po_01ABC", Amount: 100, PixKey: "12345678909"},
		"no amount":         {Ref: payoutRef, Amount: 0, PixKey: "12345678909"},
		"negative":          {Ref: payoutRef, Amount: -1, PixKey: "12345678909"},
		"no key":            {Ref: payoutRef, Amount: 100},
	} {
		if _, err := prov.Payout(context.Background(), cr, byok.Token{Value: "t"}, in); !errors.Is(err, byok.ErrInvalidRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if called {
		t.Fatal("nothing reached the bank")
	}
}

func TestAnUnknownKeyIsNotFoundAndATransportErrorIsNeverAFailure(t *testing.T) {
	prov, cr := payoutProvider(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) })
	if _, err := prov.Payout(context.Background(), cr, byok.Token{Value: "t"}, byok.PayoutRequest{Ref: payoutRef, Amount: 100, PixKey: "12345678909"}); !errors.Is(err, byok.ErrPixKeyNotFound) {
		t.Fatalf("404 on the payment is a key the bank does not have: %v", err)
	}
	prov503, cr503 := payoutProvider(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) })
	_, err := prov503.Payout(context.Background(), cr503, byok.Token{Value: "t"}, byok.PayoutRequest{Ref: payoutRef, Amount: 100, PixKey: "12345678909"})
	if !errors.Is(err, byok.ErrUnavailable) {
		t.Fatalf("a 503 says nothing about whether the money moved: %v", err)
	}
}

func TestGetPayoutReadsStatusEndToEndAndTheBanksReason(t *testing.T) {
	cases := map[string]struct {
		answer string
		status byok.PayoutStatus
		e2e    string
		reason string
	}{
		"paid":    {`{"transacaoPix":{"status":"EFETIVADO","endToEnd":"E1","codigoSolicitacao":"x"}}`, byok.PayoutDone, "E1", ""},
		"refused": {`{"transacaoPix":{"status":"REPROVADO","erros":[{"codigoErro":"1","descricaoErro":"Saldo insuficiente"}]}}`, byok.PayoutFailed, "", "Saldo insuficiente"},
		"waiting": {`{"transacaoPix":{"status":"AGUARDANDO_APROVACAO"}}`, byok.PayoutProcessing, "", ""},
		"unseen":  {`{"transacaoPix":{"status":"ALGO_NOVO"}}`, byok.PayoutUnknown, "", ""},
	}
	for name, c := range cases {
		prov, cr := payoutProvider(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/banking/v2/pix/c42f0787-02cb-4b31-827e-459ec9d7ece1" {
				t.Errorf("%s %s", r.Method, r.URL.Path)
			}
			_, _ = w.Write([]byte(c.answer))
		})
		got, err := prov.GetPayout(context.Background(), cr, byok.Token{Value: "t"}, "c42f0787-02cb-4b31-827e-459ec9d7ece1")
		if err != nil || got.Status != c.status || got.PaymentID != c.e2e || got.Reason != c.reason {
			t.Errorf("%s: %+v %v", name, got, err)
		}
	}
}

func TestAPayoutLookupThatFindsNothingIsNotFoundNotFailed(t *testing.T) {
	prov, cr := payoutProvider(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) })
	if _, err := prov.GetPayout(context.Background(), cr, byok.Token{Value: "t"}, "c42f0787-02cb-4b31-827e-459ec9d7ece1"); !errors.Is(err, byok.ErrNotFound) {
		t.Fatalf("%v: the caller decides what a payment the bank does not know means", err)
	}
}

func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var m map[string]any
	_ = json.NewDecoder(r.Body).Decode(&m)
	return m
}
