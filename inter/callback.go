package inter

import (
	"encoding/json"
	"fmt"

	"gopkg.aoctech.app/api-commons/byok"
	"gopkg.aoctech.app/api-commons/inter/pix"
)

// ParseCallback reads a Pix webhook body (Bacen: {"pix":[...]}) into
// references only. Amounts, status and payer are deliberately dropped.
func (p *Provider) ParseCallback(product byok.Product, headers map[string]string, body []byte) (byok.Callback, error) {
	if err := pixOnly(product); err != nil {
		return byok.Callback{}, err
	}
	var b pix.CallbackBody
	if err := json.Unmarshal(body, &b); err != nil {
		return byok.Callback{}, fmt.Errorf("%w: body", byok.ErrInvalidRequest)
	}
	out := byok.Callback{Account: headers["x-conta-corrente"]}
	for _, r := range b.Pix {
		if !ValidTxid(r.Txid) {
			return byok.Callback{}, fmt.Errorf("%w: txid", byok.ErrInvalidRequest)
		}
		out.ChargeRefs = append(out.ChargeRefs, r.Txid)
		if r.EndToEndID != "" {
			out.PaymentIDs = append(out.PaymentIDs, r.EndToEndID)
		}
	}
	return out, nil
}
