// Package oauth2client provides a cached OAuth2 client_credentials token
// fetcher, shared by every CTech Go service that calls another service's M2M
// token endpoint (previously duplicated independently in ctech-wallet's
// kycclient and walletclient packages).
package oauth2client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// TokenManager fetches and caches an OAuth2 client_credentials bearer token,
// refreshing 30 seconds before its reported expiry.
type TokenManager struct {
	client       *http.Client
	tokenURL     string
	clientID     string
	clientSecret string
	scope        string

	mu     sync.Mutex
	token  string
	expiry time.Time
}

// New builds a TokenManager. tokenURL is the full token endpoint URL.
func New(httpClient *http.Client, tokenURL, clientID, clientSecret, scope string) *TokenManager {
	return &TokenManager{client: httpClient, tokenURL: tokenURL, clientID: clientID, clientSecret: clientSecret, scope: scope}
}

// Get returns a cached valid bearer token, fetching a new one if absent or
// close to expiry.
func (t *TokenManager) Get(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" && time.Now().Before(t.expiry) {
		return t.token, nil
	}
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {t.clientID},
		"client_secret": {t.clientSecret},
		"scope":         {t.scope},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := t.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("oauth2client: token endpoint status %d: %s", resp.StatusCode, string(raw))
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &tr); err != nil {
		return "", err
	}
	t.token = tr.AccessToken
	t.expiry = time.Now().Add(time.Duration(tr.ExpiresIn-30) * time.Second)
	return t.token, nil
}
