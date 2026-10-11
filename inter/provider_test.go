package inter

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"gopkg.aoctech.app/api-commons/byok"
)

var _ byok.Provider = (*Provider)(nil)

// fakeInter answers the token endpoint per requested scope set and /saldo.
func fakeInter(t *testing.T, grant func(scope string) (status int, granted string)) (*Provider, byok.Credentials) {
	p := newPKI(t, time.Now().AddDate(1, 0, 0), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/v2/token":
			_ = r.ParseForm()
			status, granted := grant(r.Form.Get("scope"))
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"access_token":"t","expires_in":3600,"scope":"` + granted + `"}`))
		case "/banking/v2/saldo":
			_, _ = w.Write([]byte(`{"disponivel":10.25}`))
		default:
			w.WriteHeader(404)
		}
	}))
	prov := NewProvider(NewClient(WithBaseURL(p.server.URL), WithRootCAs(p.pool)))
	return prov, byok.Credentials{Values: map[string]string{"client_id": "c", "client_secret": "s",
		"cert_pem": string(p.clientCert), "key_pem": string(p.clientKey)}, Account: "1234567"}
}

func TestConnectWithEveryScopeIsCollectionCapable(t *testing.T) {
	prov, cr := fakeInter(t, func(scope string) (int, string) { return 200, scope })
	got, err := prov.Connect(context.Background(), cr)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Capabilities) != 2 || got.Balance.Available != 1025 || got.CertFingerprint == "" {
		t.Fatalf("%+v", got)
	}
}

func TestConnectFallsBackToAStatementOnlyIntegration(t *testing.T) {
	prov, cr := fakeInter(t, func(scope string) (int, string) {
		if strings.Contains(scope, "cobv") {
			return 400, ""
		}
		return 200, "extrato.read"
	})
	got, err := prov.Connect(context.Background(), cr)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Capabilities) != 1 || got.Capabilities[0] != byok.CapabilityStatement || len(got.MissingScopes) != 6 {
		t.Fatalf("%+v", got)
	}
}

func TestConnectWithAWrongSecretIsRefused(t *testing.T) {
	prov, cr := fakeInter(t, func(string) (int, string) { return 401, "" })
	if _, err := prov.Connect(context.Background(), cr); !errors.Is(err, byok.ErrCredentialsRefused) {
		t.Fatalf("%v", err)
	}
}

func TestATokenNarrowedByInterIsStillReadAsGranted(t *testing.T) {
	// spec § 15 item 8: Inter may narrow instead of refusing.
	prov, cr := fakeInter(t, func(string) (int, string) { return 200, "extrato.read cobv.read" })
	got, _ := prov.Connect(context.Background(), cr)
	if len(got.Capabilities) != 1 {
		t.Fatalf("a partial collection set must not be collection-capable: %+v", got)
	}
}
