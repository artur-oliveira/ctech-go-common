package oauth2client

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetCachesTokenUntilExpiry(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok-1","expires_in":3600}`))
	}))
	defer srv.Close()

	tm := New(srv.Client(), srv.URL, "client-id", "secret", "scope:a")
	t1, err := tm.Get(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t2, err := tm.Get(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if t1 != "tok-1" || t2 != "tok-1" {
		t.Fatalf("token = %q, %q, want tok-1 both times", t1, t2)
	}
	if calls != 1 {
		t.Fatalf("token endpoint called %d times, want 1 (second Get should hit cache)", calls)
	}
}

func TestGetFailsOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	tm := New(srv.Client(), srv.URL, "client-id", "wrong-secret", "scope:a")
	if _, err := tm.Get(t.Context()); err == nil {
		t.Fatal("expected an error on a 401 token response")
	}
}
