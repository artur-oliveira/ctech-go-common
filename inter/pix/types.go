// Package pix holds the Bacen Pix API types Inter speaks (cobv, recebidos,
// devolução, webhook, callbacks). Types only; calls are inter.Client methods.
package pix

import "encoding/json"

const (
	StatusAtiva             = "ATIVA"
	StatusConcluida         = "CONCLUIDA"
	StatusRemovidaRecebedor = "REMOVIDA_PELO_USUARIO_RECEBEDOR"
	StatusRemovidaPSP       = "REMOVIDA_PELO_PSP"
)

type Calendario struct {
	DataDeVencimento       string `json:"dataDeVencimento,omitempty"`
	ValidadeAposVencimento int    `json:"validadeAposVencimento,omitempty"`
	// Expiracao is an immediate charge's lifetime in seconds (cob); the due-date fields above are cobv's.
	Expiracao int `json:"expiracao,omitempty"`
}

type Devedor struct {
	Nome string `json:"nome"`
	CPF  string `json:"cpf,omitempty"`
	CNPJ string `json:"cnpj,omitempty"`
}

// Modalidade: multa 2 = percentual; juros 3 = percentual ao mês, dias corridos.
type Modalidade struct {
	Modalidade int    `json:"modalidade"`
	ValorPerc  string `json:"valorPerc"`
}

type Valor struct {
	Original string      `json:"original"`
	Multa    *Modalidade `json:"multa,omitempty"`
	Juros    *Modalidade `json:"juros,omitempty"`
}

type CobvRequest struct {
	Calendario         Calendario `json:"calendario"`
	Devedor            Devedor    `json:"devedor"`
	Valor              Valor      `json:"valor"`
	Chave              string     `json:"chave"`
	SolicitacaoPagador string     `json:"solicitacaoPagador,omitempty"`
}

type ComponenteValor struct {
	Valor string `json:"valor"`
}

type ComponentesValor struct {
	Original *ComponenteValor `json:"original,omitempty"`
	Multa    *ComponenteValor `json:"multa,omitempty"`
	Juros    *ComponenteValor `json:"juros,omitempty"`
}

type Recebido struct {
	EndToEndID       string            `json:"endToEndId"`
	Txid             string            `json:"txid"`
	Valor            string            `json:"valor"`
	Horario          string            `json:"horario"`
	ComponentesValor *ComponentesValor `json:"componentesValor,omitempty"`
	// Pagador is who paid, as the webhook names them (observed live on
	// 2026-10-15): the name in full and the CPF/CNPJ masked to its middle digits
	// ("***137303**"). The charge query does not carry it.
	Pagador *PagadorWebhook `json:"pagador,omitempty"`
}

// PagadorWebhook is the payer a Pix webhook names.
type PagadorWebhook struct {
	Nome    string `json:"nome"`
	CpfCnpj string `json:"cpfCnpj"`
}

type Cobv struct {
	Txid          string     `json:"txid"`
	Status        string     `json:"status"`
	PixCopiaECola string     `json:"pixCopiaECola"`
	Valor         Valor      `json:"valor"`
	Pix           []Recebido `json:"pix"`
}

type Devolucao struct {
	ID     string `json:"id"`
	RtrID  string `json:"rtrId"`
	Valor  string `json:"valor"`
	Status string `json:"status"`
}

type CallbackBody struct {
	Pix []Recebido `json:"pix"`
}

type CallbackRecord struct {
	WebhookURL      string          `json:"webhookUrl"`
	Payload         json.RawMessage `json:"payload"`
	NumeroTentativa int             `json:"numeroTentativa"`
	HTTPStatus      int             `json:"httpStatus"`
	DataHoraDisparo string          `json:"dataHoraDisparo"`
	Txid            string          `json:"txid"`
}
