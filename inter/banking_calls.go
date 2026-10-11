package inter

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"gopkg.aoctech.app/api-commons/byok"
)

type ExtratoLine struct {
	IDTransacao   string `json:"idTransacao"`
	DataTransacao string `json:"dataTransacao"`
	TipoOperacao  string `json:"tipoOperacao"` // C | D
	Valor         string `json:"valor"`
	Titulo        string `json:"titulo"`
	Descricao     string `json:"descricao"`
	Detalhes      struct {
		TxID       string `json:"txId"`
		EndToEndID string `json:"endToEndId"`
	} `json:"detalhes"`
}

// ExtratoCompleto reads one page (pagina/tamanhoPagina, never scroll: one
// scroll per account, and the merchant's ERP may hold it). Window ≤ 90 days.
func (c *Client) ExtratoCompleto(ctx context.Context, cr Credentials, bearer string, from, to byok.Date, page, size int) ([]ExtratoLine, int, error) {
	var out struct {
		TotalPaginas int           `json:"totalPaginas"`
		Transacoes   []ExtratoLine `json:"transacoes"`
	}
	q := url.Values{"dataInicio": {from.String()}, "dataFim": {to.String()}, "pagina": {fmt.Sprint(page)}, "tamanhoPagina": {fmt.Sprint(size)}}
	if err := c.Do(ctx, cr, bearer, http.MethodGet, "/banking/v2/extrato/completo", q, nil, &out); err != nil {
		return nil, 0, err
	}
	return out.Transacoes, out.TotalPaginas, nil
}

type Saldo struct {
	Disponivel              float64 `json:"disponivel"`
	BloqueadoCheque         float64 `json:"bloqueadoCheque"`
	BloqueadoJudicialmente  float64 `json:"bloqueadoJudicialmente"`
	BloqueadoAdministrativo float64 `json:"bloqueadoAdministrativo"`
}

func (c *Client) Saldo(ctx context.Context, cr Credentials, bearer string, at byok.Date) (Saldo, error) {
	var out Saldo
	q := url.Values{}
	if !at.IsZero() {
		q.Set("dataSaldo", at.String())
	}
	err := c.Do(ctx, cr, bearer, http.MethodGet, "/banking/v2/saldo", q, nil, &out)
	return out, err
}
