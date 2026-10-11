package inter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"gopkg.aoctech.app/api-commons/byok"
)

const (
	ProductionURL = "https://cdpj.partners.bancointer.com.br"
	SandboxURL    = "https://cdpj-sandbox.partners.uatinter.co"
	pathToken     = "/oauth/v2/token"
)

var (
	ScopesStatement  = []string{"extrato.read"}
	ScopesCollection = []string{"cobv.read", "cobv.write", "pix.read", "pix.write", "webhook.read", "webhook.write"}
)

// Credentials are one integration's, for one call.
type Credentials struct {
	ClientID, ClientSecret string
	CertPEM, KeyPEM        []byte
	Account, PixKey        string
	Sandbox                bool
}

// CredentialsFrom reads the plaintext Values of a byok.Credentials.
func CredentialsFrom(c byok.Credentials) (Credentials, error) {
	v := c.Values
	if v["client_id"] == "" || v["client_secret"] == "" || v["cert_pem"] == "" || v["key_pem"] == "" {
		return Credentials{}, fmt.Errorf("%w: client_id, client_secret, cert_pem and key_pem are required", byok.ErrInvalidRequest)
	}
	return Credentials{ClientID: v["client_id"], ClientSecret: v["client_secret"], CertPEM: []byte(v["cert_pem"]),
		KeyPEM: []byte(v["key_pem"]), Account: c.Account, PixKey: c.PixKey, Sandbox: c.Sandbox}, nil
}

type CertInfo struct {
	Subject, Fingerprint string
	NotAfter             time.Time
}

// InspectPair parses and checks a certificate and key without any network call.
func InspectPair(certPEM, keyPEM []byte, now time.Time) (CertInfo, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return CertInfo{}, fmt.Errorf("%w: not a PEM certificate", byok.ErrCertificateInvalid)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return CertInfo{}, fmt.Errorf("%w: %v", byok.ErrCertificateInvalid, err)
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		if kb, _ := pem.Decode(keyPEM); kb == nil {
			return CertInfo{}, fmt.Errorf("%w: not a PEM key", byok.ErrCertificateInvalid)
		}
		return CertInfo{}, byok.ErrKeyMismatch
	}
	sum := sha256.Sum256(cert.Raw)
	info := CertInfo{Subject: cert.Subject.String(), Fingerprint: hex.EncodeToString(sum[:]), NotAfter: cert.NotAfter.UTC()}
	if !now.Before(cert.NotAfter) {
		return info, byok.ErrCertificateExpired
	}
	return info, nil
}

type Option func(*Client)

func WithBaseURL(u string) Option         { return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") } }
func WithRootCAs(p *x509.CertPool) Option { return func(c *Client) { c.roots = p } }
func WithTimeout(d time.Duration) Option  { return func(c *Client) { c.timeout = d } }

// Client caches one *http.Transport per client-certificate fingerprint, so a
// warm process keeps its TLS sessions per integration and a renewal (a new
// certificate) gets a new transport. Transports idle 30 min are dropped.
type Client struct {
	baseURL string // empty: chosen per call from Credentials.Sandbox
	roots   *x509.CertPool
	timeout time.Duration
	mu      sync.Mutex
	pool    map[string]*entry
}

type entry struct {
	rt   *http.Transport
	used time.Time
}

func NewClient(opts ...Option) *Client {
	c := &Client{timeout: 20 * time.Second, pool: map[string]*entry{}}
	for _, o := range opts {
		o(c)
	}
	return c
}

func (c *Client) transports() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.pool) }

func (c *Client) base(cr Credentials) string {
	switch {
	case c.baseURL != "":
		return c.baseURL
	case cr.Sandbox:
		return SandboxURL
	}
	return ProductionURL
}

func (c *Client) httpFor(cr Credentials) (*http.Client, error) {
	sum := sha256.Sum256(append(append([]byte{}, cr.CertPEM...), cr.KeyPEM...))
	key := hex.EncodeToString(sum[:])
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.pool {
		if now.Sub(e.used) > 30*time.Minute {
			e.rt.CloseIdleConnections()
			delete(c.pool, k)
		}
	}
	if e, ok := c.pool[key]; ok {
		e.used = now
		return &http.Client{Transport: e.rt, Timeout: c.timeout}, nil
	}
	pair, err := tls.X509KeyPair(cr.CertPEM, cr.KeyPEM)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", byok.ErrCertificateInvalid, err)
	}
	rt := &http.Transport{
		TLSClientConfig:     &tls.Config{Certificates: []tls.Certificate{pair}, RootCAs: c.roots, MinVersion: tls.VersionTLS12},
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
	}
	c.pool[key] = &entry{rt: rt, used: now}
	return &http.Client{Transport: rt, Timeout: c.timeout}, nil
}

// FetchToken asks for a client_credentials bearer with exactly these scopes.
func (c *Client) FetchToken(ctx context.Context, cr Credentials, scopes []string) (byok.Token, error) {
	hc, err := c.httpFor(cr)
	if err != nil {
		return byok.Token{}, err
	}
	form := url.Values{"client_id": {cr.ClientID}, "client_secret": {cr.ClientSecret},
		"grant_type": {"client_credentials"}, "scope": {strings.Join(scopes, " ")}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base(cr)+pathToken, strings.NewReader(form.Encode()))
	if err != nil {
		return byok.Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return byok.Token{}, fmt.Errorf("%w: token: %v", byok.ErrUnavailable, sanitize(err))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		// Inter answers 400/401 to a wrong secret and to scopes the integration lacks.
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return byok.Token{}, fmt.Errorf("%w: token %d %s", byok.ErrCredentialsRefused, resp.StatusCode, problemTitle(raw))
		}
		return byok.Token{}, statusErr(http.MethodPost, pathToken, resp.StatusCode, raw)
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Scope       string `json:"scope"`
	}
	if err := json.Unmarshal(raw, &tr); err != nil || tr.AccessToken == "" {
		return byok.Token{}, fmt.Errorf("%w: token body", byok.ErrUnavailable)
	}
	return byok.Token{Value: tr.AccessToken, ExpiresIn: tr.ExpiresIn, Scopes: strings.Fields(tr.Scope)}, nil
}

// Do sends one API call with the bearer; x-conta-corrente when Account is set.
func (c *Client) Do(ctx context.Context, cr Credentials, bearer, method, path string, query url.Values, body, out any) error {
	hc, err := c.httpFor(cr)
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	u := c.base(cr) + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cr.Account != "" {
		req.Header.Set("x-conta-corrente", cr.Account)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s %s: %v", byok.ErrUnavailable, method, path, sanitize(err))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return statusErr(method, path, resp.StatusCode, raw)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("%w: %s %s: body", byok.ErrUnavailable, method, path)
		}
	}
	return nil
}
