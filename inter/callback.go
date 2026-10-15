package inter

import (
	"encoding/json"
	"fmt"

	"gopkg.aoctech.app/api-commons/byok"
	"gopkg.aoctech.app/api-commons/inter/pix"
)

// ParseCallback reads a Pix webhook body (Bacen: {"pix":[...]}) into
// references and, when the body names one, the payer of each payment (a claim,
// usually with a masked document). Amounts and status are deliberately dropped.
func (p *Provider) ParseCallback(product byok.Product, headers map[string]string, body []byte) (byok.Callback, error) {
	if product == byok.ProductPixPayment {
		return parsePayoutCallback(headers, body), nil
	}
	if err := pixOnly(product); err != nil {
		return byok.Callback{}, err
	}
	var b pix.CallbackBody
	if err := json.Unmarshal(body, &b); err != nil {
		return byok.Callback{}, fmt.Errorf("%w: body", byok.ErrInvalidRequest)
	}
	out := byok.Callback{Account: headers["x-conta-corrente"]}
	for _, r := range b.Pix {
		// A Pix received on the key without a txid (a plain transfer) or a
		// devolução notice names no charge: skip it, keep the rest of the batch.
		if r.Txid == "" {
			continue
		}
		if !ValidTxid(r.Txid) {
			return byok.Callback{}, fmt.Errorf("%w: txid", byok.ErrInvalidRequest)
		}
		out.ChargeRefs = append(out.ChargeRefs, r.Txid)
		if r.EndToEndID != "" {
			out.PaymentIDs = append(out.PaymentIDs, r.EndToEndID)
		}
		if r.Pagador != nil && (r.Pagador.Nome != "" || r.Pagador.CpfCnpj != "") {
			out.Payers = append(out.Payers, byok.CallbackPayer{
				ChargeRef: r.Txid, PaymentID: r.EndToEndID, Name: r.Pagador.Nome, Document: r.Pagador.CpfCnpj,
			})
		}
	}
	return out, nil
}

// parsePayoutCallback reads the notice of an outgoing Pix payment. Its body
// was not documented where it could be read when this was written, and the
// useful property of the notice is only that something happened to a payment this
// account sent, so it is read leniently: the account from the header, and any
// payment ids (codigoSolicitacao, endToEndId) found at the top of an object or of
// each object of a list. Nothing in it settles anything: the consumer asks the
// bank. A body that is not JSON still yields the account, so the notice is
// acted on rather than lost.
func parsePayoutCallback(headers map[string]string, body []byte) byok.Callback {
	out := byok.Callback{Account: headers["x-conta-corrente"]}
	var objects []map[string]any
	var one map[string]any
	var many []map[string]any
	switch {
	case json.Unmarshal(body, &many) == nil:
		objects = many
	case json.Unmarshal(body, &one) == nil:
		objects = []map[string]any{one}
	}
	for _, o := range objects {
		if v, ok := o["codigoSolicitacao"].(string); ok && v != "" {
			out.PaymentIDs = append(out.PaymentIDs, v)
		}
		if v, ok := o["endToEndId"].(string); ok && v != "" {
			out.PaymentIDs = append(out.PaymentIDs, v)
		}
	}
	return out
}
