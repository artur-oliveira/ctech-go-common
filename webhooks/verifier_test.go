package webhooks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func verifierFor(t *testing.T, h http.HandlerFunc) (*Verifier, string) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	v := NewVerifier("test/1")
	v.SetHTTPClient(srv.Client())
	return v, srv.URL + "/hook?keep=1"
}

func TestVerifierAcceptsAnEndpointThatEchoesTheChallenge(t *testing.T) {
	v, url := verifierFor(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.Method != http.MethodGet || q.Get("hub.mode") != "subscribe" || q.Get("keep") != "1" {
			t.Errorf("request = %s %s", r.Method, r.URL)
		}
		_, _ = w.Write([]byte(q.Get("hub.challenge") + "\n"))
	})
	if err := v.Verify(context.Background(), url); err != nil {
		t.Fatal(err)
	}
}

func TestVerifierRejectsWrongBodyNon200AndRedirect(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"wrong body": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) },
		"not found":  func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) },
		"echo+extra": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(r.URL.Query().Get("hub.challenge") + "x"))
		},
		"redirect": func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/elsewhere", http.StatusFound) },
	} {
		t.Run(name, func(t *testing.T) {
			v, url := verifierFor(t, h)
			if err := v.Verify(context.Background(), url); err == nil {
				t.Fatal("verified an endpoint that did not return the challenge")
			}
		})
	}
}

func TestVerifierUsesAFreshChallengeEachTime(t *testing.T) {
	seen := map[string]bool{}
	v, url := verifierFor(t, func(w http.ResponseWriter, r *http.Request) {
		c := r.URL.Query().Get("hub.challenge")
		seen[c] = true
		if len(c) != 64 {
			t.Errorf("challenge length = %d", len(c))
		}
		_, _ = w.Write([]byte(c))
	})
	for i := 0; i < 3; i++ {
		if err := v.Verify(context.Background(), url); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 3 {
		t.Fatalf("challenges = %v", seen)
	}
}

func TestVerifierRefusesAPrivateAddressByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.URL.Query().Get("hub.challenge")))
	}))
	defer srv.Close()
	if err := NewVerifier("test/1").Verify(context.Background(), srv.URL); err == nil {
		t.Fatal("the guarded client reached 127.0.0.1")
	}
}
