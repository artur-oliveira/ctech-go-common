package webhooks

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"
)

func TestCheckURL(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want error // nil, ErrInvalidURL or ErrHostNotAllowed
	}{
		{"https://hooks.example.com/billing", nil},
		{"https://example.com:8443/x?y=1", nil},
		{"https://203.0.113.10/hook", nil},
		{"http://hooks.example.com/billing", ErrInvalidURL},
		{"hooks.example.com/billing", ErrInvalidURL},
		{"https://user:pw@hooks.example.com/", ErrInvalidURL},
		{"https:///nohost", ErrInvalidURL},
		{"https://127.0.0.1/hook", ErrHostNotAllowed},
		{"https://[::1]/hook", ErrHostNotAllowed},
		{"https://10.0.0.5/hook", ErrHostNotAllowed},
		{"https://169.254.169.254/latest/meta-data", ErrHostNotAllowed},
		{"https://100.64.1.1/hook", ErrHostNotAllowed},
		{"https://[::ffff:192.168.0.1]/hook", ErrHostNotAllowed},
		{"https://localhost/hook", ErrHostNotAllowed},
		{"https://api.localhost/hook", ErrHostNotAllowed},
		{"https://printer.local/hook", ErrHostNotAllowed},
		{"https://dfe.internal/hook", ErrHostNotAllowed},
		{"https://intranet/hook", ErrHostNotAllowed},
	} {
		err := CheckURL(tc.url)
		switch {
		case tc.want == nil && err != nil:
			t.Errorf("%s: %v, want accepted", tc.url, err)
		case tc.want != nil && !errors.Is(err, tc.want):
			t.Errorf("%s: %v, want %v", tc.url, err, tc.want)
		}
	}
}

func TestPublicAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "2001:4860:4860::8888": true,
		"127.0.0.1": false, "10.1.2.3": false, "172.16.0.1": false, "192.168.1.1": false,
		"169.254.169.254": false, "100.64.0.1": false, "0.0.0.0": false, "::1": false,
		"fe80::1": false, "fc00::1": false, "224.0.0.1": false, "::ffff:10.0.0.1": false,
		"0.1.2.3": false, "192.0.0.8": false, "198.18.0.1": false, "240.0.0.1": false,
		"64:ff9b::a00:1": false, "64:ff9b:1::1": false, "2002:a00:1::1": false, "2001::1": false,
	} {
		if got := PublicAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("PublicAddr(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestGuardedClientRefusesLoopback(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(srv.Close)
	_, err := NewGuardedClient(time.Second).Get(srv.URL)
	if !errors.Is(err, ErrPrivateAddress) {
		t.Fatalf("err = %v, want ErrPrivateAddress", err)
	}
	if hits.Load() != 0 {
		t.Fatal("the request reached the private address")
	}
}

func TestGuardedClientFollowsNoRedirectAndNoProxy(t *testing.T) {
	c := NewGuardedClient(time.Second)
	if c.CheckRedirect == nil || !errors.Is(c.CheckRedirect(nil, nil), http.ErrUseLastResponse) {
		t.Fatal("the client follows redirects")
	}
	if tr, ok := c.Transport.(*http.Transport); !ok || tr.Proxy != nil {
		t.Fatal("the client may go through a proxy, which the dial guard would not see")
	}
}
