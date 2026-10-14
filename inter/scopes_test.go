package inter

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.aoctech.app/api-commons/byok"
)

// documentedScopes is every OAuth scope Inter's API reference lists
// (cobrança, pagamento, pix and pix automático, read 2026-10-14). A scope Inter
// adds later fails TestEveryDocumentedScopeBelongsToOneSet until it is placed.
var documentedScopes = []string{
	"boleto-cobranca.read", "boleto-cobranca.write",
	"extrato.read",
	"pagamento-boleto.read", "pagamento-boleto.write", "pagamento-darf.write",
	"pagamento-lote.read", "pagamento-lote.write", "pagamento-pix.read", "pagamento-pix.write",
	"webhook-banking.read", "webhook-banking.write",
	"cob.read", "cob.write", "cobv.read", "cobv.write", "lotecobv.read", "lotecobv.write",
	"payloadlocation.read", "payloadlocation.write", "pix.read", "pix.write", "webhook.read", "webhook.write",
	"rec.read", "rec.write", "solicrec.read", "solicrec.write", "cobr.read", "cobr.write",
	"payloadlocationrec.read", "payloadlocationrec.write", "webhookrec.read", "webhookrec.write",
	"webhookcobr.read", "webhookcobr.write",
}

func TestEveryDocumentedScopeBelongsToOneSet(t *testing.T) {
	seen := map[string]string{}
	for name, set := range map[string][]string{
		"statement": ScopesStatement, "collection": ScopesCollection, "immediate": ScopesImmediateCharge,
		"due_batch": ScopesDueBatch, "boleto": ScopesBoleto, "recurring": ScopesRecurring,
		"payout": ScopesPayout, "bank_payments": ScopesBankPayments,
	} {
		for _, s := range set {
			if prev, dup := seen[s]; dup {
				t.Errorf("%s is in %s and %s", s, prev, name)
			}
			seen[s] = name
		}
	}
	for _, s := range documentedScopes {
		if seen[s] == "" {
			t.Errorf("%s is documented by Inter but belongs to no set", s)
		}
	}
	for s := range seen {
		if !slices.Contains(documentedScopes, s) {
			t.Errorf("%s is in a set but Inter does not document it", s)
		}
	}
	if !slices.Equal(AllScopes(), slices.Sorted(slices.Values(documentedScopes))) {
		t.Errorf("AllScopes is the sorted documented list: %v", AllScopes())
	}
}

func TestBankPaymentsAreNeverRequestedByConnect(t *testing.T) {
	var asked []string
	prov, cr := fakeInter(t, func(scope string) (int, string) { asked = append(asked, scope); return 200, scope })
	if _, err := prov.Connect(context.Background(), cr); err != nil {
		t.Fatal(err)
	}
	for _, s := range ScopesBankPayments {
		if strings.Contains(asked[0], s) {
			t.Errorf("connect asked for %s: boleto, DARF and batch payments are explicit, never ambient", s)
		}
	}
}

func TestConnectAsksForEveryProductAtOnceAndReportsEachOne(t *testing.T) {
	var asked []string
	prov, cr := fakeInter(t, func(scope string) (int, string) { asked = append(asked, scope); return 200, scope })
	got, err := prov.Connect(context.Background(), cr)
	if err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 {
		t.Fatalf("one token request when everything is ticked: %v", asked)
	}
	for _, want := range []byok.Capability{byok.CapabilityStatement, byok.CapabilityCollection, byok.CapabilityImmediateCharge,
		byok.CapabilityDueBatch, byok.CapabilityBoleto, byok.CapabilityRecurring, byok.CapabilityPayout} {
		if !slices.Contains(got.Capabilities, want) {
			t.Errorf("capability %s missing from %v", want, got.Capabilities)
		}
	}
	if len(got.MissingScopes) != 0 {
		t.Errorf("nothing is missing: %v", got.MissingScopes)
	}
}

func TestConnectFallsBackToCollectionWhenTheOtherProductsAreNotTicked(t *testing.T) {
	// A tenant that ticked the original five activities only: the first request
	// (everything) is refused, the second (statement + collection) is not.
	prov, cr := fakeInter(t, func(scope string) (int, string) {
		for _, optional := range []string{"boleto-cobranca", "pagamento-pix", "rec.", "cob.read", "lotecobv", "webhook-banking"} {
			if strings.Contains(scope, optional) {
				return 400, ""
			}
		}
		return 200, scope
	})
	got, err := prov.Connect(context.Background(), cr)
	if err != nil {
		t.Fatal(err)
	}
	want := []byok.Capability{byok.CapabilityStatement, byok.CapabilityCollection}
	if !slices.Equal(got.Capabilities, want) || len(got.MissingScopes) != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestATokenForOneProductAsksForThatProductsScopesOnly(t *testing.T) {
	cases := map[byok.Capability][]string{
		byok.CapabilityStatement: ScopesStatement, byok.CapabilityCollection: ScopesCollection,
		byok.CapabilityImmediateCharge: ScopesImmediateCharge, byok.CapabilityDueBatch: ScopesDueBatch,
		byok.CapabilityBoleto: ScopesBoleto, byok.CapabilityRecurring: ScopesRecurring, byok.CapabilityPayout: ScopesPayout,
	}
	for capability, scopes := range cases {
		var asked string
		p := newPKI(t, time.Now().AddDate(1, 0, 0), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			asked = r.Form.Get("scope")
			_, _ = w.Write([]byte(`{"access_token":"t","expires_in":3600}`))
		}))
		prov := NewProvider(NewClient(WithBaseURL(p.server.URL), WithRootCAs(p.pool)))
		cr := byok.Credentials{Values: map[string]string{"client_id": "c", "client_secret": "s", "cert_pem": string(p.clientCert), "key_pem": string(p.clientKey)}}
		if _, err := prov.Token(context.Background(), cr, capability); err != nil {
			t.Fatal(err)
		}
		if strings.Fields(asked)[0] == "" || !slices.Equal(strings.Fields(asked), scopes) {
			t.Errorf("%s asked %q, want %v: a collection token must never carry payout power", capability, asked, scopes)
		}
	}
}
