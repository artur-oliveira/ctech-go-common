// Package accountorgs is the M2M client for ctech-account's organization
// facts: whether a person belongs to an organization (and with which role),
// and which organizations a person belongs to.
//
//	GET /v1.0/internal/organizations/{org}/members/{user}  scope internal:account:org-member
//	GET /v1.0/internal/users/{user}/organizations          scope internal:account:user-organizations
//
// Extracted from ctech-billing (internal/accountclient/membership.go) and
// ctech-dfe (internal/accountclient/workspace.go), which had grown two copies
// of the same client with the return values of Membership in different orders.
//
// It fails closed by construction: a nil client, a token failure, a non-200,
// an unreadable body and an outage all return an error, and callers that
// authorize on Membership must read an error as a refusal. "Not a member" is
// an answer (Membership{}, nil), never an error: ctech-account answers a
// non-member and an organization that does not exist alike, with a 200, so a
// 404 here means the route is missing, not that the organization is.
package accountorgs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gopkg.aoctech.app/api-commons/cache"
	"gopkg.aoctech.app/api-commons/oauth2client"
)

// MemberScope is what the membership route requires.
const MemberScope = "internal:account:org-member"

// ListScope is what listing a user's organizations requires: its own scope at
// ctech-account, because enumerating a person's workspaces is a wider grant
// than checking one membership. It is minted on its own token so a missing
// grant for it degrades the listing, never the membership checks.
const ListScope = "internal:account:user-organizations"

// Workspace kinds as ctech-account reports them. An absent kind is an
// organization: ctech-account sent none before personal workspaces existed.
const (
	KindOrganization = "organization"
	KindPersonal     = "personal"
)

// IsOrganizationKind reports a raw kind that is an organization: absent or
// "organization". A personal workspace, or a kind added later, is not.
func IsOrganizationKind(kind string) bool { return kind == "" || kind == KindOrganization }

const (
	requestTimeout = 6 * time.Second
	// maxMembershipBody caps a single membership answer.
	maxMembershipBody = 8 << 10
	// maxListBody caps the organization list: a person in many organizations
	// outgrows the 8 KiB that suits a single membership answer.
	maxListBody = 256 << 10
)

// Config is what the client needs to reach ctech-account. Cache backs the
// token managers (pass the service's Valkey backend so replicas share tokens;
// nil falls back to a per-process cache).
type Config struct {
	BaseURL      string
	TokenURL     string
	ClientID     string
	ClientSecret string
	Cache        cache.Backend
}

// Client asks ctech-account about organization membership. One token manager
// per scope, so a grant missing for the list never breaks the role check.
type Client struct {
	http         *http.Client
	memberTokens *oauth2client.TokenManager // MemberScope
	listTokens   *oauth2client.TokenManager // ListScope
	baseURL      string
}

// New builds a client, or returns nil when the credential is not configured.
// A nil client is NOT permissive: every call on nil is an error.
func New(cfg Config) *Client {
	if cfg.BaseURL == "" || cfg.TokenURL == "" || cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil
	}
	hc := &http.Client{Timeout: requestTimeout}
	return &Client{
		http:         hc,
		memberTokens: oauth2client.New(hc, cfg.Cache, cfg.TokenURL, cfg.ClientID, cfg.ClientSecret, MemberScope),
		listTokens:   oauth2client.New(hc, cfg.Cache, cfg.TokenURL, cfg.ClientID, cfg.ClientSecret, ListScope),
		baseURL:      strings.TrimSuffix(cfg.BaseURL, "/"),
	}
}

// Enabled reports a client that can actually ask.
func (c *Client) Enabled() bool { return c != nil }

// Membership is ctech-account's answer about one person in one organization.
// A refusal is the zero value: it carries no role and no kind (ctech-account
// never tells a non-member which ids are personal workspaces).
type Membership struct {
	Member bool   `json:"member"`
	Role   string `json:"role"`
	// Kind is the workspace's, raw: "organization", "personal", or "" from a
	// ctech-account that predates kinds. Normalise with IsOrganizationKind.
	Kind string `json:"kind"`
}

// IsOrganization reports a membership of an organization (absent kind
// included). Meaningless on a refusal; check Member first.
func (m Membership) IsOrganization() bool { return IsOrganizationKind(m.Kind) }

// Membership reports whether userID belongs to organizationID (a workspace of
// either kind), with which ctech-account role and the workspace's raw kind.
// (Membership{}, nil) is a refusal; any error is an outage the caller must
// refuse on.
func (c *Client) Membership(ctx context.Context, organizationID, userID string) (Membership, error) {
	if c == nil {
		return Membership{}, fmt.Errorf("ctech-account membership client is not configured")
	}
	token, err := c.memberTokens.Get(ctx)
	if err != nil {
		return Membership{}, fmt.Errorf("minting a service token: %w", err)
	}
	return c.membershipWithToken(ctx, token, organizationID, userID)
}

// membershipWithToken is the request itself, split from the token so the HTTP
// behaviour is testable without a token endpoint.
func (c *Client) membershipWithToken(ctx context.Context, token, organizationID, userID string) (Membership, error) {
	path := fmt.Sprintf("%s/v1.0/internal/organizations/%s/members/%s",
		c.baseURL, url.PathEscape(organizationID), url.PathEscape(userID))
	var out Membership
	if err := c.getJSON(ctx, token, path, maxMembershipBody, &out); err != nil {
		return Membership{}, fmt.Errorf("reading the membership: %w", err)
	}
	if !out.Member {
		// A refusal carries nothing, even if a body said more.
		return Membership{}, nil
	}
	return out, nil
}

// Organization is one workspace a person belongs to, as ctech-account lists it.
type Organization struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	// Kind is raw: "" before ctech-account sent kinds (IsOrganizationKind).
	Kind string `json:"kind"`
	// People (members, owner excluded) and PendingInvitations are sent only on
	// personal workspaces the user owns. Absent is "not known", never zero.
	People             *int `json:"people,omitempty"`
	PendingInvitations *int `json:"pending_invitations,omitempty"`
}

// IsOrganization reports an organization (absent kind included).
func (o Organization) IsOrganization() bool { return IsOrganizationKind(o.Kind) }

// Organizations lists the organizations userID belongs to, with their role in
// each. It is information only: authorize with Membership, so a stale or
// generous list grants nothing. Like Membership it fails closed: a nil client,
// a non-200 and an unreadable body are errors, never an empty list. An unknown
// user is an empty list (ctech-account answers it with a 200).
func (c *Client) Organizations(ctx context.Context, userID string) ([]Organization, error) {
	if c == nil {
		return nil, fmt.Errorf("ctech-account organizations client is not configured")
	}
	token, err := c.listTokens.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("minting a service token: %w", err)
	}
	return c.organizationsWithToken(ctx, token, userID)
}

func (c *Client) organizationsWithToken(ctx context.Context, token, userID string) ([]Organization, error) {
	path := fmt.Sprintf("%s/v1.0/internal/users/%s/organizations", c.baseURL, url.PathEscape(userID))
	var out struct {
		Organizations []Organization `json:"organizations"`
	}
	if err := c.getJSON(ctx, token, path, maxListBody, &out); err != nil {
		return nil, fmt.Errorf("reading the organizations: %w", err)
	}
	return out.Organizations, nil
}

// getJSON is the request both calls make: bearer token, 200 or error, capped
// body.
func (c *Client) getJSON(ctx context.Context, token, path string, limit int64, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return fmt.Errorf("building the request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("calling ctech-account: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// Including 403 and 404: a 403 is this client's own credential being
		// wrong (an operational fault, not a statement about the user), and a
		// 404 is the route missing. Reading either as a refusal would hide a
		// broken deployment behind thousands of denied requests.
		return fmt.Errorf("ctech-account answered %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, limit)).Decode(out); err != nil {
		return fmt.Errorf("decoding the answer: %w", err)
	}
	return nil
}
