package inter

import (
	"context"
	"encoding/json"
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
		TxID               string `json:"txId"`
		EndToEndID         string `json:"endToEndId"`
		NomePagador        string `json:"nomePagador"`
		CPFCNPJPagador     string `json:"cpfCnpjPagador"`
		NomeEmpresaPagador string `json:"nomeEmpresaPagador"`
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

const pathBankingPix = "/banking/v2/pix"

// PixPaymentRequest pays a Pix key (the other destinations Inter accepts,
// bank data and copia-e-cola, are not used: a payout goes to a key).
type PixPaymentRequest struct {
	Valor        json.Number `json:"valor"`
	Descricao    string      `json:"descricao,omitempty"`
	Destinatario struct {
		Tipo  string `json:"tipo"`
		Chave string `json:"chave"`
	} `json:"destinatario"`
}

type PixPaymentAccepted struct {
	TipoRetorno       string `json:"tipoRetorno"`
	CodigoSolicitacao string `json:"codigoSolicitacao"`
}

// PixPayment sends the payment. idem is the RFC 4122 UUID Inter deduplicates on.
func (c *Client) PixPayment(ctx context.Context, cr Credentials, bearer, idem string, req PixPaymentRequest) (PixPaymentAccepted, error) {
	var out PixPaymentAccepted
	err := c.DoHeaders(ctx, cr, bearer, http.MethodPost, pathBankingPix, nil, map[string]string{"x-id-idempotente": idem}, req, &out)
	return out, err
}

type PixPaymentView struct {
	TransacaoPix struct {
		Status            string `json:"status"`
		EndToEnd          string `json:"endToEnd"`
		CodigoSolicitacao string `json:"codigoSolicitacao"`
		Erros             []struct {
			CodigoErro    string `json:"codigoErro"`
			DescricaoErro string `json:"descricaoErro"`
		} `json:"erros"`
	} `json:"transacaoPix"`
}

// GetPixPayment reads a payment by the codigoSolicitacao Inter answered with.
func (c *Client) GetPixPayment(ctx context.Context, cr Credentials, bearer, codigoSolicitacao string) (PixPaymentView, error) {
	var out PixPaymentView
	err := c.Do(ctx, cr, bearer, http.MethodGet, pathBankingPix+"/"+url.PathEscape(codigoSolicitacao), nil, nil, &out)
	return out, err
}
