package oauthclients

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serve answers every request with status, content type and body, and records
// the request URI. Tests call getWithToken, so no token endpoint is needed.
func serve(t *testing.T, status int, contentType, body string) (*Client, *string) {
	t.Helper()
	var gotURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.RequestURI
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &Client{http: srv.Client(), baseURL: srv.URL}, &gotURI
}

func TestGetReadsTheFourFieldsAndEscapesTheID(t *testing.T) {
	c, uri := serve(t, http.StatusOK, "application/json",
		`{"client_id":"a b","owner_user_id":"usr_1","allowed_scopes":["billing:invoices:read"],"active":true}`)
	got, err := c.getWithToken(context.Background(), "tok", "a b")
	if err != nil {
		t.Fatal(err)
	}
	if got.ClientID != "a b" || got.OwnerUserID != "usr_1" || !got.Active ||
		len(got.AllowedScopes) != 1 || got.AllowedScopes[0] != "billing:invoices:read" {
		t.Fatalf("got %+v", got)
	}
	if *uri != "/v1.0/internal/oauth-clients/a%20b" {
		t.Fatalf("request URI = %s", *uri)
	}
}

func TestNullScopesBecomeAnEmptyList(t *testing.T) {
	c, _ := serve(t, http.StatusOK, "application/json", `{"client_id":"c","owner_user_id":"u","allowed_scopes":null,"active":false}`)
	got, err := c.getWithToken(context.Background(), "tok", "c")
	if err != nil || got.AllowedScopes == nil || len(got.AllowedScopes) != 0 {
		t.Fatalf("got %+v, %v", got, err)
	}
}

// ctech-account answers an unknown client with an RFC 7807 404. A route that
// does not exist answers Fiber's plain-text 404: that is a broken deployment,
// and reading it as "unknown client" would refuse every merchant silently.
func TestAProblem404IsNotFoundAndAPlain404IsAnError(t *testing.T) {
	c, _ := serve(t, http.StatusNotFound, "application/problem+json; charset=utf-8", `{"status":404}`)
	if _, err := c.getWithToken(context.Background(), "tok", "c"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("problem 404: err = %v, want ErrNotFound", err)
	}
	c, _ = serve(t, http.StatusNotFound, "text/plain; charset=utf-8", "Cannot GET")
	if _, err := c.getWithToken(context.Background(), "tok", "c"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("plain 404: err = %v, want a non-NotFound error", err)
	}
}

func TestEveryOtherFailureIsAnError(t *testing.T) {
	for _, tc := range []struct {
		name, ct, body string
		status         int
	}{
		{"forbidden", "application/problem+json", `{}`, http.StatusForbidden},
		{"outage", "text/plain", "boom", http.StatusBadGateway},
		{"garbage", "application/json", "{", http.StatusOK},
		{"another client", "application/json", `{"client_id":"other","owner_user_id":"u","allowed_scopes":[],"active":true}`, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := serve(t, tc.status, tc.ct, tc.body)
			if _, err := c.getWithToken(context.Background(), "tok", "c"); err == nil || errors.Is(err, ErrNotFound) {
				t.Fatalf("err = %v, want a non-NotFound error", err)
			}
		})
	}
}

func TestANilClientIsNeverPermissive(t *testing.T) {
	var c *Client
	if c.Enabled() {
		t.Fatal("nil client enabled")
	}
	if _, err := c.Get(context.Background(), "c"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if New(Config{BaseURL: "x"}) != nil {
		t.Fatal("incomplete config built a client")
	}
}

// No creation path at ctech-account makes an id longer than 128 characters or
// an empty one; asking would cost a token and a round trip to learn "404".
func TestImpossibleIDsAreNotFoundWithoutARequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the client asked ctech-account")
	}))
	t.Cleanup(srv.Close)
	c := &Client{http: srv.Client(), baseURL: srv.URL}
	for _, id := range []string{"", strings.Repeat("a", 129)} {
		if _, err := c.Get(context.Background(), id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("id of length %d: err = %v", len(id), err)
		}
	}
}
