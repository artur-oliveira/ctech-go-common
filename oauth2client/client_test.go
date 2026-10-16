package oauth2client

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	backend "gopkg.aoctech.app/api-commons/cache"
)

func TestGetCachesTokenUntilExpiry(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok-1","expires_in":3600}`))
	}))
	defer srv.Close()

	tm := New(srv.Client(), nil, srv.URL, "client-id", "secret", "scope:a")
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

	tm := New(srv.Client(), nil, srv.URL, "client-id", "wrong-secret", "scope:a")
	if _, err := tm.Get(t.Context()); err == nil {
		t.Fatal("expected an error on a 401 token response")
	}
}

func TestGetSharesTokenAcrossInstancesViaCacheBackend(t *testing.T) {
	// Simulates a multi-instance deployment: two independent TokenManagers
	// (as if running in two replicas) backed by the same cache — e.g. Redis
	// in production. The second replica must reuse the first's token instead
	// of hitting the token endpoint again.
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok-shared","expires_in":3600}`))
	}))
	defer srv.Close()

	shared := backend.NewMemoryBackend(10)
	tm1 := New(srv.Client(), shared, srv.URL, "client-id", "secret", "scope:a")
	tm2 := New(srv.Client(), shared, srv.URL, "client-id", "secret", "scope:a")

	tok1, err := tm1.Get(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	tok2, err := tm2.Get(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if tok1 != "tok-shared" || tok2 != "tok-shared" {
		t.Fatalf("token = %q, %q, want tok-shared both times", tok1, tok2)
	}
	if calls != 1 {
		t.Fatalf("token endpoint called %d times, want 1 (second instance should hit shared cache)", calls)
	}
}

func TestAMomentaryOutageOfTheTokenEndpointIsRiddenOutButARefusalIsNot(t *testing.T) {
	retryDelay = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { retryDelay = func(n int) time.Duration { return time.Duration(n) * 300 * time.Millisecond } })

	calls := 0
	blip := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusServiceUnavailable) // "No backend server is currently available"
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"tok-2","expires_in":3600}`))
	}))
	defer blip.Close()
	got, err := New(blip.Client(), nil, blip.URL, "id", "secret", "s").Get(t.Context())
	if err != nil || got != "tok-2" || calls != 3 {
		t.Fatalf("token=%q err=%v calls=%d: two 503s then a token must succeed", got, err, calls)
	}

	refused := 0
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refused++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer deny.Close()
	if _, err := New(deny.Client(), nil, deny.URL, "id", "bad", "s").Get(t.Context()); err == nil || refused != 1 {
		t.Fatalf("a refused credential is asked once, got err=%v calls=%d", err, refused)
	}

	down := 0
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { down++; w.WriteHeader(http.StatusServiceUnavailable) }))
	defer dead.Close()
	if _, err := New(dead.Client(), nil, dead.URL, "id", "s", "s").Get(t.Context()); err == nil || down != tokenAttempts {
		t.Fatalf("a long outage gives up after %d attempts, got err=%v calls=%d", tokenAttempts, err, down)
	}
}
