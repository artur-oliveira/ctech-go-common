package inter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"gopkg.aoctech.app/api-commons/byok"
	"gopkg.aoctech.app/api-commons/inter/pix"
)

type seen struct {
	method, path, query, account string
	body                         map[string]any
}

func recorder(t *testing.T, answer string, status int) (*Client, Credentials, *[]seen) {
	t.Helper()
	var calls []seen
	p := newPKI(t, time.Now().AddDate(1, 0, 0), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := seen{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, account: r.Header.Get("x-conta-corrente")}
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			_ = json.Unmarshal(b, &s.body)
		}
		calls = append(calls, s)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(answer))
	}))
	return NewClient(WithBaseURL(p.server.URL), WithRootCAs(p.pool)),
		Credentials{CertPEM: p.clientCert, KeyPEM: p.clientKey, Account: "1234567", PixKey: "evp-key"}, &calls
}

func TestPutCobvSendsTheBacenBody(t *testing.T) {
	c, cr, calls := recorder(t, `{"txid":"ctb01JABCDEFGHJKMNPQRSTVWXYZ","status":"ATIVA","pixCopiaECola":"000201"}`, 201)
	in := pix.CobvRequest{
		Calendario: pix.Calendario{DataDeVencimento: "2026-11-10", ValidadeAposVencimento: 30},
		Devedor:    pix.Devedor{Nome: "Ana", CPF: "12345678909"},
		Valor:      pix.Valor{Original: Reais(9990), Multa: &pix.Modalidade{Modalidade: 2, ValorPerc: "2.00"}, Juros: &pix.Modalidade{Modalidade: 3, ValorPerc: "1.00"}},
		Chave:      "evp-key", SolicitacaoPagador: "Fatura #12",
	}
	got, err := c.PutCobv(context.Background(), cr, "b", "ctb01JABCDEFGHJKMNPQRSTVWXYZ", in)
	if err != nil || got.PixCopiaECola != "000201" {
		t.Fatalf("%+v %v", got, err)
	}
	s := (*calls)[0]
	if s.method != "PUT" || s.path != "/pix/v2/cobv/ctb01JABCDEFGHJKMNPQRSTVWXYZ" || s.account != "1234567" {
		t.Fatalf("%+v", s)
	}
	if s.body["chave"] != "evp-key" || s.body["valor"].(map[string]any)["original"] != "99.90" {
		t.Fatalf("body %+v", s.body)
	}
}

func TestRemoveCobvSendsOnlyTheStatus(t *testing.T) {
	c, cr, calls := recorder(t, `{}`, 200)
	if err := c.RemoveCobv(context.Background(), cr, "b", "ctb01JABCDEFGHJKMNPQRSTVWXYZ"); err != nil {
		t.Fatal(err)
	}
	s := (*calls)[0]
	if s.method != "PATCH" || len(s.body) != 1 || s.body["status"] != "REMOVIDA_PELO_USUARIO_RECEBEDOR" {
		t.Fatalf("%+v", s)
	}
}

func TestGetWebhookURLIsEmptyWhenNoneIsRegistered(t *testing.T) {
	c, cr, calls := recorder(t, `{"title":"Webhook não encontrado"}`, 404)
	url, err := c.GetWebhookURL(context.Background(), cr, "b")
	if err != nil || url != "" || (*calls)[0].path != "/pix/v2/webhook/evp-key" {
		t.Fatalf("%q %v %+v", url, err, *calls)
	}
}

func TestPaginationNeverUsesScroll(t *testing.T) {
	c, cr, calls := recorder(t, `{"totalPaginas":3,"transacoes":[{"idTransacao":"T1","dataTransacao":"2026-10-01","tipoOperacao":"C","valor":"10.50","titulo":"Pix recebido","detalhes":{"txId":"ctb01","endToEndId":"E1"}}]}`, 200)
	from, _ := byok.ParseDate("2026-07-04")
	to, _ := byok.ParseDate("2026-10-01")
	lines, total, err := c.ExtratoCompleto(context.Background(), cr, "b", from, to, 1, 500)
	if err != nil || total != 3 || lines[0].Detalhes.TxID != "ctb01" {
		t.Fatalf("%+v %d %v", lines, total, err)
	}
	q := (*calls)[0].query
	if (*calls)[0].path != "/banking/v2/extrato/completo" || q != "dataFim=2026-10-01&dataInicio=2026-07-04&pagina=1&tamanhoPagina=500" {
		t.Fatalf("query %q", q)
	}
}

func TestRetryCallbacksRefusesMoreThanFifty(t *testing.T) {
	c, cr, _ := recorder(t, `{}`, 200)
	if err := c.RetryCallbacks(context.Background(), cr, "b", make([]string, 51)); err == nil {
		t.Fatal("51 txids accepted")
	}
}

func TestCallbackListingAsksForNoMoreThanFiftyPerPage(t *testing.T) {
	// Inter: tamanhoPagina is [10..50]; 100 answered 400 in production.
	c, cr, calls := recorder(t, `{"data":[],"totalPaginas":1}`, 200)
	if _, err := c.ListCallbacks(context.Background(), cr, "b", time.Now().Add(-time.Hour), time.Now()); err != nil {
		t.Fatal(err)
	}
	if q := (*calls)[0].query; !strings.Contains(q, "tamanhoPagina=50") {
		t.Fatalf("query %q", q)
	}
}
