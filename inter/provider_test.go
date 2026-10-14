package inter

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.aoctech.app/api-commons/byok"
)

var _ byok.Provider = (*Provider)(nil)

// fakeInter answers the token endpoint per requested scope set and /saldo.
func fakeInter(t *testing.T, grant func(scope string) (status int, granted string)) (*Provider, byok.Credentials) {
	p := newPKI(t, time.Now().AddDate(1, 0, 0), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/v2/token":
			_ = r.ParseForm()
			status, granted := grant(r.Form.Get("scope"))
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"access_token":"t","expires_in":3600,"scope":"` + granted + `"}`))
		case "/banking/v2/saldo":
			_, _ = w.Write([]byte(`{"disponivel":10.25}`))
		default:
			w.WriteHeader(404)
		}
	}))
	prov := NewProvider(NewClient(WithBaseURL(p.server.URL), WithRootCAs(p.pool)))
	return prov, byok.Credentials{Values: map[string]string{"client_id": "c", "client_secret": "s",
		"cert_pem": string(p.clientCert), "key_pem": string(p.clientKey)}, Account: "1234567"}
}

func TestConnectWithEveryScopeIsCollectionCapable(t *testing.T) {
	prov, cr := fakeInter(t, func(scope string) (int, string) { return 200, scope })
	got, err := prov.Connect(context.Background(), cr)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got.Capabilities, byok.CapabilityStatement) || !slices.Contains(got.Capabilities, byok.CapabilityCollection) ||
		got.Balance.Available != 1025 || got.CertFingerprint == "" {
		t.Fatalf("%+v", got)
	}
}

func TestConnectFallsBackToAStatementOnlyIntegration(t *testing.T) {
	prov, cr := fakeInter(t, func(scope string) (int, string) {
		if strings.Contains(scope, "cobv") {
			return 400, ""
		}
		return 200, "extrato.read"
	})
	got, err := prov.Connect(context.Background(), cr)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Capabilities) != 1 || got.Capabilities[0] != byok.CapabilityStatement || len(got.MissingScopes) != 6 {
		t.Fatalf("%+v", got)
	}
}

func TestConnectWithAWrongSecretIsRefused(t *testing.T) {
	prov, cr := fakeInter(t, func(string) (int, string) { return 401, "" })
	if _, err := prov.Connect(context.Background(), cr); !errors.Is(err, byok.ErrCredentialsRefused) {
		t.Fatalf("%v", err)
	}
}

func TestATokenNarrowedByInterIsStillReadAsGranted(t *testing.T) {
	// spec § 15 item 8: Inter may narrow instead of refusing.
	prov, cr := fakeInter(t, func(string) (int, string) { return 200, "extrato.read cobv.read" })
	got, _ := prov.Connect(context.Background(), cr)
	if len(got.Capabilities) != 1 {
		t.Fatalf("a partial collection set must not be collection-capable: %+v", got)
	}
}

func TestATransientTokenFailureIsNotReadAsAStatementOnlyIntegration(t *testing.T) {
	calls := 0
	prov, cr := fakeInter(t, func(scope string) (int, string) {
		calls++
		if calls == 1 {
			return 503, ""
		}
		return 200, scope
	})
	if _, err := prov.Connect(context.Background(), cr); !errors.Is(err, byok.ErrUnavailable) {
		t.Fatalf("a 503 on the full scope set must surface, not downgrade the connection: %v", err)
	}
}

func TestAnUnknownChargeStatusIsAnErrorNotRemoved(t *testing.T) {
	c, cr, _ := recorder(t, `{"txid":"ctb01JABCDEFGHJKMNPQRSTVWXYZ","status":""}`, 200)
	prov := NewProvider(c)
	bc := byok.Credentials{Values: map[string]string{"client_id": "c", "client_secret": "s",
		"cert_pem": string(cr.CertPEM), "key_pem": string(cr.KeyPEM)}}
	if _, err := prov.GetCharge(context.Background(), bc, byok.Token{Value: "t"}, "ctb01JABCDEFGHJKMNPQRSTVWXYZ"); !errors.Is(err, byok.ErrUnavailable) {
		t.Fatalf("an unknown status must not read as removed: %v", err)
	}
}

func TestAStatementLineWithAnUnreadableAmountIsSkippedNotPostedAsZero(t *testing.T) {
	c, cr, _ := recorder(t, `{"totalPaginas":1,"transacoes":[`+
		`{"idTransacao":"T1","dataTransacao":"2026-10-01","tipoOperacao":"C","valor":"10,50"},`+
		`{"idTransacao":"T2","dataTransacao":"2026-10-01","tipoOperacao":"X","valor":"1.00"},`+
		`{"idTransacao":"T3","dataTransacao":"2026-10-01","tipoOperacao":"D","valor":"2.00"}]}`, 200)
	prov := NewProvider(c)
	bc := byok.Credentials{Values: map[string]string{"client_id": "c", "client_secret": "s",
		"cert_pem": string(cr.CertPEM), "key_pem": string(cr.KeyPEM)}}
	d, _ := byok.ParseDate("2026-10-01")
	page, err := prov.StatementPage(context.Background(), bc, byok.Token{Value: "t"}, d, d, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Lines) != 1 || page.Lines[0].ID != "T3" || page.Lines[0].Amount != 200 || page.Lines[0].Direction != byok.Debit {
		t.Fatalf("only the readable line is kept: %+v", page.Lines)
	}
}

func TestCallbackHistoryTakesTheTxidFromThePayload(t *testing.T) {
	c, cr, _ := recorder(t, `{"totalPaginas":1,"data":[{"webhookUrl":"https://h/w","numeroTentativa":2,"httpStatus":503,`+
		`"dataHoraDisparo":"2026-10-11T10:00:00Z","payload":{"pix":[{"endToEndId":"E1","txid":"ctb01JABCDEFGHJKMNPQRSTVWXYZ"}]}}]}`, 200)
	prov := NewProvider(c)
	bc := byok.Credentials{Values: map[string]string{"client_id": "c", "client_secret": "s",
		"cert_pem": string(cr.CertPEM), "key_pem": string(cr.KeyPEM)}}
	got, err := prov.ListCallbacks(context.Background(), bc, byok.Token{Value: "t"}, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ChargeRef != "ctb01JABCDEFGHJKMNPQRSTVWXYZ" || got[0].HTTPStatus != 503 || got[0].Attempt != 2 {
		t.Fatalf("%+v", got)
	}
}

func TestAReceivedPixLineCarriesItsPayer(t *testing.T) {
	c, cr, _ := recorder(t, `{"totalPaginas":1,"transacoes":[`+
		`{"idTransacao":"T1","dataTransacao":"2026-10-14","tipoOperacao":"C","valor":"0.10","titulo":"Pix recebido","descricao":"Fulano de Tal","detalhes":{"txId":"ctb01JABCDEFGHJKMNPQRSTVWXYZ","endToEndId":"E1","nomePagador":"Fulano de Tal","cpfCnpjPagador":"12345678909","nomeEmpresaPagador":"BANCO EXEMPLO"}},`+
		`{"idTransacao":"T2","dataTransacao":"2026-10-14","tipoOperacao":"C","valor":"1.00","titulo":"Pix recebido","detalhes":{"endToEndId":"E2"}},`+
		`{"idTransacao":"T3","dataTransacao":"2026-10-14","tipoOperacao":"D","valor":"2.00","titulo":"Pix enviado","detalhes":{"nomePagador":"C TECH","cpfCnpjPagador":"62787449000107"}}]}`, 200)
	prov := NewProvider(c)
	bc := byok.Credentials{Values: map[string]string{"client_id": "c", "client_secret": "s", "cert_pem": string(cr.CertPEM), "key_pem": string(cr.KeyPEM)}}
	d, _ := byok.ParseDate("2026-10-14")
	page, err := prov.StatementPage(context.Background(), bc, byok.Token{Value: "t"}, d, d, 0)
	if err != nil || len(page.Lines) != 3 {
		t.Fatalf("%+v %v", page, err)
	}
	want := byok.Payer{Name: "Fulano de Tal", Document: "12345678909", Institution: "BANCO EXEMPLO"}
	if got := page.Lines[0].Payer; got == nil || *got != want {
		t.Fatalf("payer = %+v, want %+v", got, want)
	}
	if page.Lines[1].Payer != nil {
		t.Fatalf("a line without payer detail has no payer: %+v", page.Lines[1].Payer)
	}
	if page.Lines[2].Payer != nil {
		t.Fatalf("on a debit the 'pagador' is CTech itself, never a payer: %+v", page.Lines[2].Payer)
	}
}
