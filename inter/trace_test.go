package inter

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

func traceClient(t *testing.T, buf *bytes.Buffer, h http.HandlerFunc) (*Client, Credentials) {
	t.Helper()
	p := newPKI(t, time.Now().AddDate(1, 0, 0), h)
	lg := slog.New(slog.NewJSONHandler(buf, nil))
	c := NewClient(WithBaseURL(p.server.URL), WithRootCAs(p.pool), WithLogger(lg))
	return c, Credentials{ClientID: "cid", ClientSecret: "SECRET-VALUE", CertPEM: p.clientCert, KeyPEM: p.clientKey}
}

func TestDoLogsTheRawResponseOfAFailedCall(t *testing.T) {
	var buf bytes.Buffer
	c, cr := traceClient(t, &buf, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"title":"Unauthorized","detail":"conta nao autorizada"}`))
	})
	_ = c.Do(context.Background(), cr, "b", "GET", "/banking/v2/saldo", nil, nil, nil)
	got := buf.String()
	for _, want := range []string{`"status":401`, `"path":"/banking/v2/saldo"`, `conta nao autorizada`, `"method":"GET"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("log %s lacks %s", got, want)
		}
	}
}

func TestDoLogsTheRawBodyOfASuccessfulCallToo(t *testing.T) {
	var buf bytes.Buffer
	c, cr := traceClient(t, &buf, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"disponivel":12.34,"campoNovo":"visto"}`))
	})
	var out map[string]any
	if err := c.Do(context.Background(), cr, "b", "GET", "/banking/v2/saldo", nil, nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "campoNovo") {
		t.Fatalf("a field nobody parses must still be in the trace: %s", buf.String())
	}
}

func TestTraceNeverWritesTheSecretsItHolds(t *testing.T) {
	var buf bytes.Buffer
	c, cr := traceClient(t, &buf, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/v2/token" {
			_, _ = w.Write([]byte(`{"access_token":"BEARER-VALUE","expires_in":3600,"scope":"extrato.read"}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"client_secret=SECRET-VALUE e Bearer BEARER-VALUE"}`))
	})
	if _, err := c.FetchToken(context.Background(), cr, ScopesStatement); err != nil {
		t.Fatal(err)
	}
	_ = c.Do(context.Background(), cr, "BEARER-VALUE", "GET", "/x", nil, nil, nil)
	got := buf.String()
	if strings.Contains(got, "BEARER-VALUE") || strings.Contains(got, "SECRET-VALUE") {
		t.Fatalf("a secret reached the log: %s", got)
	}
	if !strings.Contains(got, "extrato.read") {
		t.Fatalf("the rest of the token response should be there: %s", got)
	}
}

func TestTraceTruncatesALongBody(t *testing.T) {
	var buf bytes.Buffer
	c, cr := traceClient(t, &buf, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"x":"` + strings.Repeat("a", 3*maxTraceBody) + `"}`))
	})
	_ = c.Do(context.Background(), cr, "b", "GET", "/x", nil, nil, nil)
	if buf.Len() > 2*maxTraceBody || !strings.Contains(buf.String(), "truncated") {
		t.Fatalf("log of %d bytes was not truncated", buf.Len())
	}
}

func TestNoLoggerMeansNoLog(t *testing.T) {
	p := newPKI(t, time.Now().AddDate(1, 0, 0), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	c := NewClient(WithBaseURL(p.server.URL), WithRootCAs(p.pool))
	if err := c.Do(context.Background(), Credentials{CertPEM: p.clientCert, KeyPEM: p.clientKey}, "b", "GET", "/x", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
}
