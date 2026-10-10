package accountorgs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serve answers every request with status/body and records the request URI.
// The client it returns has no token managers: tests call the *WithToken
// halves, so which status means what is testable without a token endpoint.
func serve(t *testing.T, status int, body string) (*Client, *string) {
	t.Helper()
	var gotURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.RequestURI
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &Client{http: srv.Client(), baseURL: srv.URL}, &gotURI
}

func TestMembershipReadsRoleKindAndPath(t *testing.T) {
	c, uri := serve(t, http.StatusOK, `{"member":true,"role":"admin","kind":"organization"}`)
	m, err := c.membershipWithToken(context.Background(), "tok", "org-1", "user-1")
	if err != nil || m != (Membership{Member: true, Role: "admin", Kind: "organization"}) {
		t.Fatalf("got %+v, %v", m, err)
	}
	if *uri != "/v1.0/internal/organizations/org-1/members/user-1" {
		t.Fatalf("request URI = %s", *uri)
	}
}

// Before ctech-account sent kinds, a member answer had none: the kind stays
// raw ("") and the caller normalises it.
func TestMembershipWithoutKindKeepsItRaw(t *testing.T) {
	c, _ := serve(t, http.StatusOK, `{"member":true,"role":"owner"}`)
	m, err := c.membershipWithToken(context.Background(), "tok", "o", "u")
	if err != nil || m != (Membership{Member: true, Role: "owner"}) {
		t.Fatalf("got %+v, %v", m, err)
	}
}

// "Not a member" is an answer, not an error. ctech-account answers it with a
// 200 for a non-member and for an organization that does not exist alike.
func TestANonMemberIsAnAnswerNotAnError(t *testing.T) {
	c, _ := serve(t, http.StatusOK, `{"member":false}`)
	m, err := c.membershipWithToken(context.Background(), "tok", "o", "u")
	if err != nil || m != (Membership{}) {
		t.Fatalf("got %+v, %v", m, err)
	}
}

// A refusal carries nothing, even if a body said more.
func TestARefusalCarriesNothing(t *testing.T) {
	c, _ := serve(t, http.StatusOK, `{"member":false,"kind":"personal","role":"owner"}`)
	m, err := c.membershipWithToken(context.Background(), "tok", "o", "u")
	if err != nil || m != (Membership{}) {
		t.Fatalf("got %+v, %v", m, err)
	}
}

// Every non-200 is an error and never a grant: a 403 is this client's own
// credential being wrong, and a 404 is the route missing (the route answers
// "not a member" with a 200). Reading either as a refusal would hide a broken
// deployment behind thousands of denied requests.
func TestEveryNon200IsAnError(t *testing.T) {
	for _, status := range []int{401, 403, 404, 429, 500, 503} {
		c, _ := serve(t, status, `{"member":true,"role":"owner"}`)
		m, err := c.membershipWithToken(context.Background(), "tok", "o", "u")
		if err == nil || m.Member {
			t.Errorf("status %d: %+v err=%v; a non-200 must be an error and never a grant", status, m, err)
		}
		if err != nil && !strings.Contains(err.Error(), "answered") {
			t.Errorf("status %d: the error does not name the answer: %v", status, err)
		}
	}
}

func TestMalformedMembershipBodyIsAnError(t *testing.T) {
	c, _ := serve(t, http.StatusOK, `<html>`)
	if m, err := c.membershipWithToken(context.Background(), "tok", "o", "u"); err == nil || m.Member {
		t.Fatalf("%+v err=%v", m, err)
	}
}

func TestIdsAreEscapedIntoThePath(t *testing.T) {
	c, uri := serve(t, http.StatusOK, `{"member":false}`)
	_, _ = c.membershipWithToken(context.Background(), "tok", "a/../b", "u?x=1")
	if want := "/v1.0/internal/organizations/a%2F..%2Fb/members/u%3Fx=1"; *uri != want {
		t.Fatalf("request URI = %q, want %q", *uri, want)
	}
}

// An unconfigured client refuses and says so: nil is never permission.
func TestANilClientRefuses(t *testing.T) {
	var c *Client
	if c.Enabled() {
		t.Fatal("a nil client reported itself enabled")
	}
	if m, err := c.Membership(context.Background(), "o", "u"); err == nil || m.Member {
		t.Fatalf("a nil client must refuse with an error: %+v err=%v", m, err)
	}
	if orgs, err := c.Organizations(context.Background(), "u"); err == nil || orgs != nil {
		t.Fatalf("a nil client must answer an error, never a list: %v err=%v", orgs, err)
	}
}

func TestNewNeedsTheWholeCredential(t *testing.T) {
	for _, cfg := range []Config{
		{},
		{BaseURL: "https://x"},
		{BaseURL: "https://x", TokenURL: "https://x/t", ClientID: "i"},
		{BaseURL: "https://x", TokenURL: "https://x/t", ClientSecret: "s"},
	} {
		if New(cfg) != nil {
			t.Errorf("an incomplete credential produced a client: %+v", cfg)
		}
	}
	if c := New(Config{BaseURL: "https://x/", TokenURL: "https://x/t", ClientID: "i", ClientSecret: "s"}); c == nil || !c.Enabled() {
		t.Fatal("a complete credential produced no client")
	} else if c.baseURL != "https://x" {
		t.Fatalf("baseURL = %q, want the trailing slash trimmed", c.baseURL)
	}
}

func TestOrganizationsReadsTheListAndEscapesTheUser(t *testing.T) {
	c, uri := serve(t, http.StatusOK, `{"organizations":[{"id":"o1","display_name":"Acme","role":"admin","kind":"organization"},{"id":"o2","display_name":"Casa","role":"viewer","kind":"personal"}]}`)
	got, err := c.organizationsWithToken(context.Background(), "tok", "u?1")
	if err != nil || len(got) != 2 {
		t.Fatalf("got %+v, %v", got, err)
	}
	if got[0].ID != "o1" || got[0].Role != "admin" || got[0].Kind != "organization" || got[1].DisplayName != "Casa" || got[1].Kind != "personal" {
		t.Fatalf("got %+v", got)
	}
	if want := "/v1.0/internal/users/u%3F1/organizations"; *uri != want {
		t.Fatalf("request URI = %q, want %q", *uri, want)
	}
}

func TestOrganizationsFailsClosed(t *testing.T) {
	for _, status := range []int{401, 403, 404, 500, 503} {
		c, _ := serve(t, status, `{"organizations":[]}`)
		if _, err := c.organizationsWithToken(context.Background(), "tok", "u"); err == nil {
			t.Errorf("status %d was read as an empty list", status)
		}
	}
	c, _ := serve(t, http.StatusOK, `<html>`)
	if _, err := c.organizationsWithToken(context.Background(), "tok", "u"); err == nil {
		t.Error("a malformed body was read as a list")
	}
}

// People and PendingInvitations are sent only on personal workspaces the user
// owns. Absent is "not known", never zero.
func TestOrganizationsCarryCountsOnlyWhenSent(t *testing.T) {
	c, _ := serve(t, http.StatusOK, `{"organizations":[
	  {"id":"a","display_name":"Casa","role":"owner","kind":"personal","people":3,"pending_invitations":1},
	  {"id":"b","display_name":"Viagem","role":"viewer","kind":"personal"}]}`)
	got, err := c.organizationsWithToken(context.Background(), "tok", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got[0].People == nil || *got[0].People != 3 || got[0].PendingInvitations == nil || *got[0].PendingInvitations != 1 {
		t.Fatalf("owned = %+v", got[0])
	}
	if got[1].People != nil || got[1].PendingInvitations != nil {
		t.Fatalf("a space not owned has no counts: %+v", got[1])
	}
}

// A person in many organizations outgrows the 8 KiB that suits a single
// membership answer; the list has its own, larger cap.
func TestOrganizationsAcceptsAListLargerThanAMembershipAnswer(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"organizations":[`)
	for i := 0; i < 200; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"id":"org_0123456789abcdef","display_name":"Escritório de Contabilidade","role":"admin","kind":"organization"}`)
	}
	b.WriteString(`]}`)
	if b.Len() <= maxMembershipBody {
		t.Fatalf("fixture is %d bytes, want more than %d", b.Len(), maxMembershipBody)
	}
	c, _ := serve(t, http.StatusOK, b.String())
	got, err := c.organizationsWithToken(context.Background(), "tok", "u")
	if err != nil || len(got) != 200 {
		t.Fatalf("got %d organizations, err %v", len(got), err)
	}
}

func TestKinds(t *testing.T) {
	for kind, want := range map[string]bool{"": true, KindOrganization: true, KindPersonal: false, "household": false} {
		if got := IsOrganizationKind(kind); got != want {
			t.Errorf("IsOrganizationKind(%q) = %v, want %v", kind, got, want)
		}
		if got := (Organization{Kind: kind}).IsOrganization(); got != want {
			t.Errorf("Organization{Kind:%q}.IsOrganization() = %v, want %v", kind, got, want)
		}
		if got := (Membership{Member: true, Kind: kind}).IsOrganization(); got != want {
			t.Errorf("Membership{Kind:%q}.IsOrganization() = %v, want %v", kind, got, want)
		}
	}
}

// Listing a user's organizations is its own scope at ctech-account and is
// fetched with its own token: if that scope has not been granted, the
// membership checks, which authorize requests, must keep working on theirs.
func TestEachQuestionMintsItsOwnScope(t *testing.T) {
	scopes := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			_ = r.ParseForm()
			scopes[r.Form.Get("scope")]++
			_, _ = w.Write([]byte(`{"access_token":"tok","token_type":"Bearer","expires_in":3600}`))
			return
		}
		if strings.Contains(r.URL.Path, "/members/") {
			_, _ = w.Write([]byte(`{"member":false}`))
			return
		}
		_, _ = w.Write([]byte(`{"organizations":[]}`))
	}))
	t.Cleanup(srv.Close)
	c := New(Config{BaseURL: srv.URL, TokenURL: srv.URL + "/token", ClientID: "svc", ClientSecret: "s"})
	for i := 0; i < 2; i++ {
		if _, err := c.Membership(context.Background(), "o", "u"); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Organizations(context.Background(), "u"); err != nil {
			t.Fatal(err)
		}
	}
	if scopes[MemberScope] != 1 || scopes[ListScope] != 1 || len(scopes) != 2 {
		t.Fatalf("token requests by scope = %v, want one each for %q and %q (cached after)", scopes, MemberScope, ListScope)
	}
}

// A token endpoint that refuses is an error on the call, never a refusal.
func TestATokenFailureIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		t.Errorf("the resource was called without a token: %s", r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	c := New(Config{BaseURL: srv.URL, TokenURL: srv.URL + "/token", ClientID: "svc", ClientSecret: "s"})
	if m, err := c.Membership(context.Background(), "o", "u"); err == nil || m.Member {
		t.Fatalf("%+v err=%v", m, err)
	}
	if _, err := c.Organizations(context.Background(), "u"); err == nil {
		t.Fatal("want an error")
	}
}
