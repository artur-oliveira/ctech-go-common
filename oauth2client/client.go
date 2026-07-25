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

	backend "gopkg.aoctech.app/api-commons/cache"
)

// TokenManager fetches and caches an OAuth2 client_credentials bearer token,
// refreshing 30 seconds before its reported expiry.
//
// Pass a backend.RedisBackend for multi-instance deployments: replicas share
// the token through it, so only one replica hits the token endpoint per
// refresh window instead of every replica fetching its own. A nil backend
// falls back to a single-instance in-memory cache (fine for tests or a
// single-replica service, but each process then fetches independently).
type TokenManager struct {
	client       *http.Client
	cache        backend.Backend
	cacheKey     string
	tokenURL     string
	clientID     string
	clientSecret string
	scope        string

	mu     sync.Mutex
	token  string
	expiry time.Time
}

// New builds a TokenManager. tokenURL is the full token endpoint URL.
func New(
	httpClient *http.Client,
	cache backend.Backend,
	tokenURL,
	clientID,
	clientSecret,
	scope string,
) *TokenManager {
	if cache == nil {
		cache = backend.NewMemoryBackend(1)
	}
	return &TokenManager{
		client:       httpClient,
		cache:        cache,
		cacheKey:     "oauth2client:" + tokenURL + ":" + clientID + ":" + scope,
		tokenURL:     tokenURL,
		clientID:     clientID,
		clientSecret: clientSecret,
		scope:        scope,
	}
}

// cachedToken is what gets stored in the shared cache backend — the local
// token/expiry fields alone aren't enough since a *different* TokenManager
// instance (another replica) needs the expiry to know the value is still
// good without re-deriving it from a TTL the backend doesn't expose on Get.
type cachedToken struct {
	AccessToken string    `json:"access_token"`
	Expiry      time.Time `json:"expiry"`
}

// Get returns a cached valid bearer token, fetching a new one if absent or
// close to expiry. Checks the in-process copy first (no cache round trip on
// the hot path), then the shared cache backend (populated by this or any
// other replica), and only calls the token endpoint on a full miss.
func (t *TokenManager) Get(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.token != "" && time.Now().Before(t.expiry) {
		return t.token, nil
	}

	if raw, ok, err := t.cache.Get(ctx, t.cacheKey); err == nil && ok {
		var ct cachedToken
		if json.Unmarshal(raw, &ct) == nil && time.Now().Before(ct.Expiry) {
			t.token, t.expiry = ct.AccessToken, ct.Expiry
			return t.token, nil
		}
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

	if enc, err := json.Marshal(cachedToken{AccessToken: t.token, Expiry: t.expiry}); err == nil {
		_ = t.cache.Set(ctx, t.cacheKey, enc, tr.ExpiresIn-30)
	}

	return t.token, nil
}
