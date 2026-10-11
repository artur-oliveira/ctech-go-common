package inter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"gopkg.aoctech.app/api-commons/byok"
	"gopkg.aoctech.app/api-commons/inter/pix"
)

const (
	pathCobv          = "/pix/v2/cobv/%s"
	pathCobvPay       = "/pix/v2/cobv/pagar/%s"
	pathDevolucao     = "/pix/v2/pix/%s/devolucao/%s"
	pathWebhook       = "/pix/v2/webhook/%s"
	pathCallbacks     = "/pix/v2/webhook/callbacks"
	pathCallbackRetry = "/pix/v2/webhook/callbacks/retry"
	maxRetryTxids     = 50
)

func (c *Client) PutCobv(ctx context.Context, cr Credentials, bearer, txid string, in pix.CobvRequest) (pix.Cobv, error) {
	if !ValidTxid(txid) {
		return pix.Cobv{}, fmt.Errorf("%w: txid", byok.ErrInvalidRequest)
	}
	var out pix.Cobv
	err := c.Do(ctx, cr, bearer, http.MethodPut, fmt.Sprintf(pathCobv, txid), nil, in, &out)
	return out, err
}

func (c *Client) GetCobv(ctx context.Context, cr Credentials, bearer, txid string) (pix.Cobv, error) {
	var out pix.Cobv
	err := c.Do(ctx, cr, bearer, http.MethodGet, fmt.Sprintf(pathCobv, txid), nil, nil, &out)
	return out, err
}

func (c *Client) PatchCobvAmount(ctx context.Context, cr Credentials, bearer, txid string, cents int64) (pix.Cobv, error) {
	var out pix.Cobv
	err := c.Do(ctx, cr, bearer, http.MethodPatch, fmt.Sprintf(pathCobv, txid), nil,
		map[string]any{"valor": map[string]string{"original": Reais(cents)}}, &out)
	return out, err
}

// RemoveCobv sends the status alone, as Bacen requires.
func (c *Client) RemoveCobv(ctx context.Context, cr Credentials, bearer, txid string) error {
	return c.Do(ctx, cr, bearer, http.MethodPatch, fmt.Sprintf(pathCobv, txid), nil,
		map[string]string{"status": pix.StatusRemovidaRecebedor}, nil)
}

func (c *Client) PutDevolucao(ctx context.Context, cr Credentials, bearer, e2e, id string, cents int64, reason string) (pix.Devolucao, error) {
	var out pix.Devolucao
	body := map[string]string{"valor": Reais(cents), "natureza": "ORIGINAL"}
	if reason != "" {
		body["descricao"] = reason
	}
	err := c.Do(ctx, cr, bearer, http.MethodPut, fmt.Sprintf(pathDevolucao, e2e, id), nil, body, &out)
	return out, err
}

func (c *Client) GetDevolucao(ctx context.Context, cr Credentials, bearer, e2e, id string) (pix.Devolucao, error) {
	var out pix.Devolucao
	err := c.Do(ctx, cr, bearer, http.MethodGet, fmt.Sprintf(pathDevolucao, e2e, id), nil, nil, &out)
	return out, err
}

func (c *Client) GetWebhookURL(ctx context.Context, cr Credentials, bearer string) (string, error) {
	var out struct {
		WebhookURL string `json:"webhookUrl"`
	}
	err := c.Do(ctx, cr, bearer, http.MethodGet, fmt.Sprintf(pathWebhook, url.PathEscape(cr.PixKey)), nil, nil, &out)
	if errors.Is(err, byok.ErrNotFound) {
		return "", nil
	}
	return out.WebhookURL, err
}

func (c *Client) PutWebhookURL(ctx context.Context, cr Credentials, bearer, hook string) error {
	return c.Do(ctx, cr, bearer, http.MethodPut, fmt.Sprintf(pathWebhook, url.PathEscape(cr.PixKey)), nil,
		map[string]string{"webhookUrl": hook}, nil)
}

func (c *Client) DeleteWebhookURL(ctx context.Context, cr Credentials, bearer string) error {
	err := c.Do(ctx, cr, bearer, http.MethodDelete, fmt.Sprintf(pathWebhook, url.PathEscape(cr.PixKey)), nil, nil, nil)
	if errors.Is(err, byok.ErrNotFound) {
		return nil
	}
	return err
}

func (c *Client) ListCallbacks(ctx context.Context, cr Credentials, bearer string, from, to time.Time) ([]pix.CallbackRecord, error) {
	var out struct {
		Data         []pix.CallbackRecord `json:"data"`
		TotalPaginas int                  `json:"totalPaginas"`
	}
	var all []pix.CallbackRecord
	for page := 0; ; page++ {
		q := url.Values{"dataHoraInicio": {from.UTC().Format(time.RFC3339)}, "dataHoraFim": {to.UTC().Format(time.RFC3339)},
			"pagina": {fmt.Sprint(page)}, "tamanhoPagina": {"100"}}
		if err := c.Do(ctx, cr, bearer, http.MethodGet, pathCallbacks, q, nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Data...)
		if page+1 >= out.TotalPaginas {
			return all, nil
		}
	}
}

func (c *Client) RetryCallbacks(ctx context.Context, cr Credentials, bearer string, txids []string) error {
	if len(txids) == 0 || len(txids) > maxRetryTxids {
		return fmt.Errorf("%w: 1..%d txids per retry", byok.ErrInvalidRequest, maxRetryTxids)
	}
	return c.Do(ctx, cr, bearer, http.MethodPost, pathCallbackRetry, nil, map[string][]string{"txids": txids}, nil)
}

func (c *Client) PaySandboxCobv(ctx context.Context, cr Credentials, bearer, txid string) error {
	if !cr.Sandbox {
		return byok.ErrSandboxOnly
	}
	return c.Do(ctx, cr, bearer, http.MethodPost, fmt.Sprintf(pathCobvPay, txid), nil, map[string]string{}, nil)
}
