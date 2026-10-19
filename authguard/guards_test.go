package authguard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"gopkg.aoctech.app/api-commons/cache"
	"gopkg.aoctech.app/api-commons/jwtverify"
)

const (
	routeOK = "/ok"
	fixedTS = 1_800_000_000
)

type down struct{ cache.Backend }

var errDown = errors.New("valkey down")

func (down) Get(context.Context, string) ([]byte, bool, error) { return nil, false, errDown }

// run mounts guard behind a middleware that sets cl and returns status and decoded body.
func run(t *testing.T, cl *jwtverify.Claims, method string, guard fiber.Handler) (int, map[string]any, http.Header) {
	t.Helper()
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		if cl != nil {
			SetClaims(c, cl)
		}
		return c.Next()
	})
	app.Use(guard)
	app.All(routeOK, func(c fiber.Ctx) error { return c.SendStatus(http.StatusOK) })
	resp, err := app.Test(httptest.NewRequest(method, routeOK, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body, resp.Header
}

func TestScopeGuards(t *testing.T) {
	user := &jwtverify.Claims{Sub: "u", SID: "s", Scope: "a b"}
	m2m := &jwtverify.Claims{Sub: "svc", Scope: "internal:x"}
	tests := []struct {
		name  string
		cl    *jwtverify.Claims
		guard fiber.Handler
		want  int
	}{
		{"scope ok", user, RequireScope("a"), 200},
		{"scope missing", user, RequireScope("z"), 403},
		{"scope no claims", nil, RequireScope("a"), 401},
		{"any ok", user, RequireAnyScope("z", "b"), 200},
		{"any none", user, RequireAnyScope("y", "z"), 403},
		{"any empty denies", user, RequireAnyScope(), 403},
		{"all ok", user, RequireAllScopes("a", "b"), 200},
		{"all partial", user, RequireAllScopes("a", "z"), 403},
		{"all empty denies", user, RequireAllScopes(), 403},
		{"all no claims", nil, RequireAllScopes("a"), 401},
		{"internal ok", m2m, RequireInternalScope("internal:x"), 200},
		{"internal user token refused", &jwtverify.Claims{SID: "s", Scope: "internal:x"}, RequireInternalScope("internal:x"), 403},
		{"internal missing scope", m2m, RequireInternalScope("internal:y"), 403},
		{"internal no claims", nil, RequireInternalScope("internal:x"), 401},
		{"client ok", &jwtverify.Claims{AZP: "web"}, RequireClientID("api", "web"), 200},
		{"client wrong", &jwtverify.Claims{AZP: "other"}, RequireClientID("web"), 403},
		{"client empty azp vs empty id", &jwtverify.Claims{}, RequireClientID(""), 403},
		{"client no claims", nil, RequireClientID("web"), 401},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, _ := run(t, tt.cl, http.MethodGet, tt.guard)
			if got != tt.want {
				t.Fatalf("status = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestScopeChallengeHeader(t *testing.T) {
	_, _, h := run(t, &jwtverify.Claims{}, http.MethodGet, RequireAnyScope("a", "b"))
	if got, want := h.Get(fiber.HeaderWWWAuthenticate), `Bearer error="insufficient_scope", scope="a b"`; got != want {
		t.Fatalf("WWW-Authenticate = %q, want %q", got, want)
	}
}

func TestRequireRecentMFA(t *testing.T) {
	now = func() time.Time { return time.Unix(fixedTS, 0) }
	t.Cleanup(func() { now = time.Now })
	const maxAge = 5 * time.Minute
	tests := []struct {
		name    string
		cl      *jwtverify.Claims
		maxAge  time.Duration
		want    int
		wantMax float64
	}{
		{"fresh", &jwtverify.Claims{LastMFAAt: fixedTS - 60}, maxAge, 200, 0},
		{"exact boundary", &jwtverify.Claims{LastMFAAt: fixedTS - 300}, maxAge, 200, 0},
		{"stale", &jwtverify.Claims{LastMFAAt: fixedTS - 301}, maxAge, 403, 300},
		{"absent claim", &jwtverify.Claims{}, maxAge, 403, 300},
		{"future skew is fresh", &jwtverify.Claims{LastMFAAt: fixedTS + 30}, maxAge, 200, 0},
		{"default max age", &jwtverify.Claims{}, 0, 403, 300},
		{"custom max age", &jwtverify.Claims{LastMFAAt: fixedTS - 120}, time.Minute, 403, 60},
		{"no claims", nil, maxAge, 401, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, body, _ := run(t, tt.cl, http.MethodPost, RequireRecentMFA(tt.maxAge))
			if got != tt.want {
				t.Fatalf("status = %d, want %d", got, tt.want)
			}
			if tt.want == 403 {
				if body["type"] != TypeStepUpRequired || body["max_age_seconds"] != tt.wantMax {
					t.Fatalf("body = %v", body)
				}
			}
		})
	}
}

func TestRevocationPolicy(t *testing.T) {
	mem := cache.NewMemoryBackend(16)
	cl := &jwtverify.Claims{Sub: "u1", IssuedAt: 100}
	if err := jwtverify.Revoke(context.Background(), mem, "u1", time.Unix(200, 0), time.Minute); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		backend cache.Backend
		cl      *jwtverify.Claims
		method  string
		guard   func(cache.Backend) fiber.Handler
		want    int
	}{
		{"revoked read", mem, cl, http.MethodGet, func(b cache.Backend) fiber.Handler { return RequireUnrevoked(b, PolicyReadOpen) }, 401},
		{"revoked write", mem, cl, http.MethodPost, func(b cache.Backend) fiber.Handler { return RequireUnrevoked(b, PolicyWriteClosed) }, 401},
		{"newer token ok", mem, &jwtverify.Claims{Sub: "u1", IssuedAt: 300}, http.MethodPost, func(b cache.Backend) fiber.Handler { return RequireUnrevoked(b, PolicyWriteClosed) }, 200},
		{"outage read-open passes", down{}, cl, http.MethodGet, func(b cache.Backend) fiber.Handler { return RequireUnrevoked(b, PolicyReadOpen) }, 200},
		{"outage write-closed 503", down{}, cl, http.MethodPost, func(b cache.Backend) fiber.Handler { return RequireUnrevoked(b, PolicyWriteClosed) }, 503},
		{"by method GET outage", down{}, cl, http.MethodGet, RequireUnrevokedByMethod, 200},
		{"by method DELETE outage", down{}, cl, http.MethodDelete, RequireUnrevokedByMethod, 503},
		{"nil backend disabled", nil, cl, http.MethodPost, RequireUnrevokedByMethod, 200},
		{"no claims", mem, nil, http.MethodGet, RequireUnrevokedByMethod, 401},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, body, _ := run(t, tt.cl, tt.method, tt.guard(tt.backend))
			if got != tt.want {
				t.Fatalf("status = %d, want %d", got, tt.want)
			}
			if got == 503 && body["type"] != TypeServiceUnavailable {
				t.Fatalf("body = %v", body)
			}
		})
	}
}

func TestPolicyForMethod(t *testing.T) {
	for m, want := range map[string]Policy{
		http.MethodGet: PolicyReadOpen, http.MethodHead: PolicyReadOpen, http.MethodOptions: PolicyReadOpen,
		http.MethodPost: PolicyWriteClosed, http.MethodPut: PolicyWriteClosed, http.MethodPatch: PolicyWriteClosed,
		http.MethodDelete: PolicyWriteClosed, "WEIRD": PolicyWriteClosed,
	} {
		if got := PolicyForMethod(m); got != want {
			t.Errorf("%s: got %d want %d", m, got, want)
		}
	}
}
