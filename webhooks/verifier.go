package webhooks

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Verifier proves a customer's endpoint is theirs and answers, before it is
// registered, the way hub.subscribe does: a GET carrying a random challenge
// that the endpoint must send back as the whole response body.
//
// The challenge is generated per attempt and is not the signing secret (which
// is shown once, after registration). The request goes through the guarded
// client: a host that resolves to a private address, a redirect or a proxy
// never gets it.
type Verifier struct {
	client    *http.Client
	userAgent string
}

// NewVerifier builds a Verifier that identifies itself with userAgent.
func NewVerifier(userAgent string) *Verifier {
	return &Verifier{client: NewGuardedClient(DeliveryTimeout), userAgent: userAgent}
}

// SetHTTPClient replaces the guarded client. Tests only.
func (v *Verifier) SetHTTPClient(c *http.Client) { v.client = c }

// maxChallengeResponse bounds what is read back: a challenge is 64 characters.
const maxChallengeResponse = 1024

// Verify returns nil when rawURL answered 200 with the challenge it was sent.
// The error text is safe to show the person registering the endpoint.
func (v *Verifier) Verify(ctx context.Context, rawURL string) error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Errorf("generating challenge: %w", err)
	}
	challenge := hex.EncodeToString(raw)

	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("the URL is not valid")
	}
	q := u.Query()
	q.Set("hub.mode", "subscribe")
	q.Set("hub.challenge", challenge)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("the URL is not valid")
	}
	req.Header.Set("User-Agent", v.userAgent)
	res, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("the endpoint did not answer")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("the endpoint answered HTTP %d, expected 200 with the challenge", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxChallengeResponse))
	if err != nil {
		return fmt.Errorf("the endpoint did not answer")
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(string(body))), []byte(challenge)) != 1 {
		return fmt.Errorf("the endpoint did not return the hub.challenge value")
	}
	return nil
}
