package problem

import (
	"net/http"
	"testing"
)

func TestRegistry(t *testing.T) {
	r := NewRegistry("https://accounts.aoctech.app/problems") // no trailing slash
	uri := r.Register("no-billing-account", http.StatusForbidden, "No Billing Account")
	if want := "https://accounts.aoctech.app/problems/no-billing-account"; uri != want || r.URI("no-billing-account") != want {
		t.Fatalf("uri = %q", uri)
	}
	if r.Register("no-billing-account", http.StatusForbidden, "No Billing Account") != uri {
		t.Fatal("idempotent re-register changed the uri")
	}

	p := r.New("no-billing-account", "detail")
	if p.Status != 403 || p.Type != uri || p.Title != "No Billing Account" || p.Code != "no_billing_account" || p.Detail != "detail" {
		t.Fatalf("problem = %+v", p)
	}
	if d, ok := r.Lookup(uri); !ok || d.Slug != "no-billing-account" {
		t.Fatalf("lookup = %+v %v", d, ok)
	}
	for _, miss := range []string{"https://other/problems/no-billing-account", r.URI("unregistered")} {
		if _, ok := r.Lookup(miss); ok {
			t.Errorf("Lookup(%q) should miss", miss)
		}
	}
}

func TestRegistryDefaultBase(t *testing.T) {
	if got := NewRegistry("").Register("x", 400, "X"); got != DefaultBaseURI+"x" {
		t.Fatalf("got %q", got)
	}
}

func TestRegistryPanics(t *testing.T) {
	r := NewRegistry("")
	r.Register("a", 400, "A")
	for name, fn := range map[string]func(){
		"conflicting register": func() { r.Register("a", 409, "A") },
		"unregistered new":     func() { r.New("nope", "") },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s did not panic", name)
				}
			}()
			fn()
		}()
	}
}

func TestCodeFromType(t *testing.T) {
	for in, want := range map[string]string{
		TypeNotFound:             "not_found",
		"https://h/problems/a-b": "a_b",
		"plain":                  "plain",
	} {
		if got := CodeFromType(in); got != want {
			t.Errorf("CodeFromType(%q) = %q, want %q", in, got, want)
		}
	}
}
