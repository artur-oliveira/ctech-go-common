package inter

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.aoctech.app/api-commons/byok"
)

func TestFetchTokenPostsTheFormOverMTLS(t *testing.T) {
	var got string
	p := newPKI(t, time.Now().AddDate(1, 0, 0), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/v2/token" || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "no", 400)
			return
		}
		_ = r.ParseForm()
		got = r.Form.Get("client_id") + "|" + r.Form.Get("grant_type") + "|" + r.Form.Get("scope")
		_, _ = w.Write([]byte(`{"access_token":"abc","token_type":"Bearer","expires_in":3600,"scope":"extrato.read"}`))
	}))
	c := NewClient(WithBaseURL(p.server.URL), WithRootCAs(p.pool))
	tok, err := c.FetchToken(context.Background(), Credentials{ClientID: "cid", ClientSecret: "s", CertPEM: p.clientCert, KeyPEM: p.clientKey}, ScopesStatement)
	if err != nil {
		t.Fatal(err)
	}
	if got != "cid|client_credentials|extrato.read" || tok.Value != "abc" || tok.ExpiresIn != 3600 || tok.Scopes[0] != "extrato.read" {
		t.Fatalf("form %q token %+v", got, tok)
	}
}

func TestStatusesMapToByokErrors(t *testing.T) {
	cases := map[int]error{401: byok.ErrUnauthorized, 403: byok.ErrScopesRefused, 404: byok.ErrNotFound,
		409: byok.ErrConcluded, 429: byok.ErrRateLimited, 503: byok.ErrUnavailable, 400: byok.ErrInvalidRequest}
	for status, want := range cases {
		p := newPKI(t, time.Now().AddDate(1, 0, 0), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"title":"x","status":` + itoa(status) + `,"detail":"client_secret=LEAK"}`))
		}))
		c := NewClient(WithBaseURL(p.server.URL), WithRootCAs(p.pool))
		err := c.Do(context.Background(), Credentials{CertPEM: p.clientCert, KeyPEM: p.clientKey}, "b", "GET", "/x", nil, nil, nil)
		if !errors.Is(err, want) {
			t.Fatalf("%d -> %v, want %v", status, err, want)
		}
		if strings.Contains(err.Error(), "LEAK") {
			t.Fatal("a provider body is never copied into an error")
		}
	}
}

func TestTheTransportIsReusedPerCertificateAndRenewedOnANewOne(t *testing.T) {
	var conns atomic.Int32
	p := newPKI(t, time.Now().AddDate(1, 0, 0), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }),
		func(s *http.Server) {
			s.ConnState = func(_ net.Conn, st http.ConnState) {
				if st == http.StateNew {
					conns.Add(1)
				}
			}
		})
	c := NewClient(WithBaseURL(p.server.URL), WithRootCAs(p.pool))
	cr := Credentials{CertPEM: p.clientCert, KeyPEM: p.clientKey}
	for range 5 {
		if err := c.Do(context.Background(), cr, "b", "GET", "/x", nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if conns.Load() != 1 {
		t.Fatalf("%d connections for one certificate, want 1 (keep-alive)", conns.Load())
	}
	if c.transports() != 1 {
		t.Fatal("one transport per fingerprint")
	}
}

func TestInspectPairRefusesExpiredMismatchedAndGarbage(t *testing.T) {
	ok := newPKI(t, time.Now().AddDate(1, 0, 0), http.NotFoundHandler())
	other := newPKI(t, time.Now().AddDate(1, 0, 0), http.NotFoundHandler())
	old := newPKI(t, time.Now().Add(-time.Hour), http.NotFoundHandler())
	if info, err := InspectPair(ok.clientCert, ok.clientKey, time.Now()); err != nil || info.Fingerprint == "" || info.Subject == "" {
		t.Fatalf("valid pair: %+v %v", info, err)
	}
	if _, err := InspectPair(ok.clientCert, other.clientKey, time.Now()); !errors.Is(err, byok.ErrKeyMismatch) {
		t.Fatalf("mismatch: %v", err)
	}
	if _, err := InspectPair(old.clientCert, old.clientKey, time.Now()); !errors.Is(err, byok.ErrCertificateExpired) {
		t.Fatalf("expired: %v", err)
	}
	if _, err := InspectPair([]byte("nope"), ok.clientKey, time.Now()); !errors.Is(err, byok.ErrCertificateInvalid) {
		t.Fatalf("garbage: %v", err)
	}
}
func itoa(i int) string { return strconv.Itoa(i) }
