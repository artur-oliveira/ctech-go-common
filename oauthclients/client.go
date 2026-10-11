// Package oauthclients is the M2M client for ctech-account's read of one OAuth
// client:
//
//	GET /v1.0/internal/oauth-clients/{client_id}  scope internal:account:oauth-client-read
//
// A product calls it before it accepts a client as somebody's credential, to
// learn who owns it, what it was granted and whether it can still authenticate
// as itself. The secret never crosses this route.
//
// It fails closed like accountorgs: a nil client, a token failure, an outage,
// a non-200 other than a problem+json 404, an unreadable body and an answer
// about another client are errors. An unknown client is ErrNotFound, told apart
// from a missing route by the problem+json body ctech-account answers it with.
package oauthclients

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gopkg.aoctech.app/api-commons/cache"
	"gopkg.aoctech.app/api-commons/oauth2client"
)

// ReadScope is what the route requires.
const ReadScope = "internal:account:oauth-client-read"

const (
	requestTimeout = 6 * time.Second
	maxBody        = 64 << 10
	// maxClientIDLength is the longest id any ctech-account creation path makes.
	maxClientIDLength = 128
)

// ErrNotFound is an unknown client (or an id no creation path can produce).
var ErrNotFound = errors.New("oauth client not found")

// Config is what the client needs to reach ctech-account.
type Config struct {
	BaseURL      string
	TokenURL     string
	ClientID     string
	ClientSecret string
	Cache        cache.Backend
}

// Client reads OAuth clients from ctech-account.
type Client struct {
	http    *http.Client
	tokens  *oauth2client.TokenManager
	baseURL string
}

// OAuthClient is ctech-account's answer. AllowedScopes is never nil.
type OAuthClient struct {
	ClientID      string   `json:"client_id"`
	OwnerUserID   string   `json:"owner_user_id"`
	AllowedScopes []string `json:"allowed_scopes"`
	Active        bool     `json:"active"`
}

// New builds a client, or returns nil when the credential is not configured.
// A nil client is NOT permissive: every call on nil is an error.
func New(cfg Config) *Client {
	if cfg.BaseURL == "" || cfg.TokenURL == "" || cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil
	}
	hc := &http.Client{Timeout: requestTimeout}
	return &Client{
		http:    hc,
		tokens:  oauth2client.New(hc, cfg.Cache, cfg.TokenURL, cfg.ClientID, cfg.ClientSecret, ReadScope),
		baseURL: strings.TrimSuffix(cfg.BaseURL, "/"),
	}
}

// Enabled reports a client that can actually ask.
func (c *Client) Enabled() bool { return c != nil }

// Get reads one client. ErrNotFound for an unknown client; any other error is
// an outage the caller must refuse on.
func (c *Client) Get(ctx context.Context, clientID string) (OAuthClient, error) {
	if c == nil {
		return OAuthClient{}, fmt.Errorf("ctech-account oauth client reader is not configured")
	}
	if clientID == "" || len(clientID) > maxClientIDLength {
		return OAuthClient{}, ErrNotFound
	}
	token, err := c.tokens.Get(ctx)
	if err != nil {
		return OAuthClient{}, fmt.Errorf("minting a service token: %w", err)
	}
	return c.getWithToken(ctx, token, clientID)
}

func (c *Client) getWithToken(ctx context.Context, token, clientID string) (OAuthClient, error) {
	path := fmt.Sprintf("%s/v1.0/internal/oauth-clients/%s", c.baseURL, url.PathEscape(clientID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return OAuthClient{}, fmt.Errorf("building the request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return OAuthClient{}, fmt.Errorf("calling ctech-account: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound && isProblem(resp.Header.Get("Content-Type")):
		return OAuthClient{}, ErrNotFound
	case resp.StatusCode != http.StatusOK:
		return OAuthClient{}, fmt.Errorf("ctech-account answered %d", resp.StatusCode)
	}
	var out OAuthClient
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&out); err != nil {
		return OAuthClient{}, fmt.Errorf("decoding the answer: %w", err)
	}
	if out.ClientID != clientID {
		return OAuthClient{}, fmt.Errorf("ctech-account answered for client %q, asked about %q", out.ClientID, clientID)
	}
	if out.AllowedScopes == nil {
		out.AllowedScopes = []string{}
	}
	return out, nil
}

func isProblem(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && mt == "application/problem+json"
}
