package webhooks

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

var (
	// ErrInvalidURL reports a URL that cannot be a webhook endpoint.
	ErrInvalidURL = errors.New("invalid webhook url")
	// ErrHostNotAllowed reports a URL whose host is not on the public internet.
	ErrHostNotAllowed = errors.New("webhook host is not a public address")
	// ErrPrivateAddress reports a dial refused because the host resolved to a
	// non-public address.
	ErrPrivateAddress = errors.New("webhook host resolves to a non-public address")
)

// nonPublic are special-purpose ranges Go's IsGlobalUnicast still accepts:
// shared address space (CGNAT), "this network", IETF protocol assignments,
// benchmarking, reserved class E, and the IPv6 translation prefixes (NAT64,
// local-use NAT64, 6to4, Teredo) through which an IPv6 address can carry a
// private IPv4 one.
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("2001::/32"),
}

// globalIPv6 is the only IPv6 block allocated to global unicast. Anything
// else (IPv4-compatible ::a.b.c.d, site-local fec0::/10, unallocated space)
// is refused even where Go calls it global unicast.
var globalIPv6 = netip.MustParsePrefix("2000::/3")

// PublicAddr reports an address a customer's endpoint may resolve to: global
// unicast, and none of private, loopback, link-local, the ranges in nonPublic,
// or an IPv4-mapped form of those.
func PublicAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	if ip.Is6() && !globalIPv6.Contains(ip) {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// CheckURL is the registration rule for a customer endpoint: https, a host, no
// credentials in the URL, and a host that names the public internet.
// Resolution is checked again at dial time (NewGuardedClient), where DNS can no
// longer change the answer.
func CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return fmt.Errorf("%w: %q is not a URL", ErrInvalidURL, raw)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("%w: %q is not https", ErrInvalidURL, raw)
	}
	if u.User != nil {
		return fmt.Errorf("%w: credentials do not belong in the URL", ErrInvalidURL)
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if ip, err := netip.ParseAddr(host); err == nil {
		if !PublicAddr(ip) {
			return fmt.Errorf("%w: %s", ErrHostNotAllowed, host)
		}
		return nil
	}
	if !strings.Contains(host, ".") || host == "localhost" {
		return fmt.Errorf("%w: %s", ErrHostNotAllowed, host)
	}
	for _, suffix := range []string{".localhost", ".local", ".internal"} {
		if strings.HasSuffix(host, suffix) {
			return fmt.Errorf("%w: %s", ErrHostNotAllowed, host)
		}
	}
	return nil
}

// NewGuardedClient returns an HTTP client for customer-registered endpoints.
// Its dialer refuses any non-public address after DNS resolution, so neither a
// hostname pointed at 10.x nor a DNS answer that changes after the registration
// check reaches the private network; it follows no redirect (a 3xx is a failed
// attempt) and uses no proxy.
func NewGuardedClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, Control: refusePrivate}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = dialer.DialContext
	return &http.Client{
		Timeout:       timeout,
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// refusePrivate runs after resolution, on the address actually dialled.
func refusePrivate(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("webhook dial %q: %w", address, err)
	}
	if !PublicAddr(ap.Addr()) {
		return ErrPrivateAddress
	}
	return nil
}
