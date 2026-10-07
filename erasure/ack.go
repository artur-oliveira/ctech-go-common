package erasure

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"gopkg.aoctech.app/api-commons/oauth2client"
)

// AckClient reports a finished user.erase to ctech-account with the service's
// client_credentials token (scope account:erasure:ack).
type AckClient struct {
	http   *http.Client
	url    string
	tokens *oauth2client.TokenManager
}

func NewAckClient(httpClient *http.Client, ackURL string, tokens *oauth2client.TokenManager) *AckClient {
	return &AckClient{http: httpClient, url: ackURL, tokens: tokens}
}

// Send POSTs a. Any non-2xx is an error, so the consumer leaves the message
// for redelivery and the (idempotent) purge runs again.
func (c *AckClient) Send(ctx context.Context, a Ack) error {
	body, err := json.Marshal(a)
	if err != nil {
		return err
	}
	token, err := c.tokens.Get(ctx)
	if err != nil {
		return fmt.Errorf("erasure: ack token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("erasure: ack %s: %w", a.RequestID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("erasure: ack %s rejected: status %d: %s", a.RequestID, resp.StatusCode, msg)
	}
	return nil
}
