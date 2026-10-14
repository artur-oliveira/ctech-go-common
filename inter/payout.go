package inter

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"gopkg.aoctech.app/api-commons/byok"
)

var _ byok.Payouts = (*Provider)(nil)

// Inter wants the idempotency id as a lowercase RFC 4122 UUID.
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// payoutStatus maps the bank's status text. PAGO and ENVIADO were observed on a live
// payout (2026-10-14): ENVIADO is sent and not yet confirmed, PAGO carries the end-to-end id. Inter's reference does not list its
// values, so only the ones that clearly mean paid or refused are decisive; the
// rest that clearly mean "in progress" are processing; anything unseen is
// unknown and a caller raises it rather than guessing about money.
func payoutStatus(raw string) byok.PayoutStatus {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "EFETIVADO", "REALIZADO", "PAGO", "PIX_PAGO", "CONCLUIDO", "CONCLUIDA", "SUCESSO":
		return byok.PayoutDone
	case "REPROVADO", "REJEITADO", "CANCELADO", "NAO_REALIZADO", "NEGADO", "ERRO", "FALHA", "EXPIRADO", "DEVOLVIDO":
		return byok.PayoutFailed
	case "AGUARDANDO_APROVACAO", "APROVACAO", "PROCESSANDO", "EM_PROCESSAMENTO", "AGENDADO", "PENDENTE", "AGUARDANDO_PAGAMENTO",
		"ENVIADO", "PIX_ENVIADO", "TRANSACAO_CRIADA", "TRANSACAO_APROVADA":
		return byok.PayoutProcessing
	}
	return byok.PayoutUnknown
}

func (p *Provider) Payout(ctx context.Context, c byok.Credentials, t byok.Token, in byok.PayoutRequest) (byok.Payout, error) {
	if !uuidPattern.MatchString(in.Ref) || in.Amount <= 0 || strings.TrimSpace(in.PixKey) == "" {
		return byok.Payout{}, fmt.Errorf("%w: payout needs a UUID ref, a positive amount and a key", byok.ErrInvalidRequest)
	}
	cr, err := p.creds(c)
	if err != nil {
		return byok.Payout{}, err
	}
	req := PixPaymentRequest{Valor: jsonNumber(Reais(in.Amount)), Descricao: in.Message}
	req.Destinatario.Tipo, req.Destinatario.Chave = "CHAVE", strings.TrimSpace(in.PixKey)
	got, err := p.c.PixPayment(ctx, cr, t.Value, in.Ref, req)
	if err != nil {
		if errors.Is(err, byok.ErrNotFound) {
			return byok.Payout{}, fmt.Errorf("%w: %v", byok.ErrPixKeyNotFound, err)
		}
		return byok.Payout{}, err
	}
	// Accepted is not settled: the outcome is read with GetPayout.
	return byok.Payout{Ref: in.Ref, ProviderRef: got.CodigoSolicitacao, Status: byok.PayoutProcessing}, nil
}

func (p *Provider) GetPayout(ctx context.Context, c byok.Credentials, t byok.Token, providerRef string) (byok.Payout, error) {
	cr, err := p.creds(c)
	if err != nil {
		return byok.Payout{}, err
	}
	v, err := p.c.GetPixPayment(ctx, cr, t.Value, providerRef)
	if err != nil {
		return byok.Payout{}, err
	}
	out := byok.Payout{ProviderRef: providerRef, PaymentID: v.TransacaoPix.EndToEnd, Status: payoutStatus(v.TransacaoPix.Status)}
	if out.Status == byok.PayoutFailed && len(v.TransacaoPix.Erros) > 0 {
		out.Reason = v.TransacaoPix.Erros[0].DescricaoErro
	}
	return out, nil
}
