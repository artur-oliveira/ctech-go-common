package ratelimit

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// ForwardedForHeader is the header the Fiber helper reads by default.
const ForwardedForHeader = "X-Forwarded-For"

// IPExtractor resolves the real client IP behind trusted proxies. Header
// values are attacker-controlled, so it never trusts X-Forwarded-For blindly:
// the header is read only when the TCP peer is itself a trusted proxy, and the
// list is walked right to left (each trusted proxy appends the address it saw)
// until the first address that is not a trusted proxy. Whatever the client
// wrote to the left of that is ignored, which is what defeats spoofing.
//
// A nil or zero-trust extractor (no proxies configured) always returns the
// TCP peer address.
type IPExtractor struct {
	trusted []netip.Prefix
}

// NewIPExtractor trusts the given proxies, as CIDRs or bare IPs (a load
// balancer's subnet, "10.0.0.0/8"). Invalid entries are an error: a typo must
// not silently turn into "trust nobody" or "trust everybody".
func NewIPExtractor(trustedProxies ...string) (*IPExtractor, error) {
	e := &IPExtractor{}
	for _, s := range trustedProxies {
		s = strings.TrimSpace(s)
		if p, err := netip.ParsePrefix(s); err == nil {
			e.trusted = append(e.trusted, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("ratelimit: invalid trusted proxy %q", s)
		}
		a = a.Unmap()
		e.trusted = append(e.trusted, netip.PrefixFrom(a, a.BitLen()))
	}
	return e, nil
}

func (e *IPExtractor) isTrusted(a netip.Addr) bool {
	for _, p := range e.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// ClientIP returns the client address for a connection from remoteAddr
// ("host:port" or a bare IP) carrying the given X-Forwarded-For header
// value(s) (pass each header line; they are read as one list in order). It
// returns "" when remoteAddr is not an IP.
func (e *IPExtractor) ClientIP(remoteAddr string, forwardedFor ...string) string {
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return ""
	}
	peer = peer.Unmap()
	if e == nil || !e.isTrusted(peer) {
		return peer.String()
	}
	var hops []string
	for _, v := range forwardedFor {
		hops = append(hops, strings.Split(v, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			return peer.String() // malformed hop: stop trusting the chain
		}
		a = a.Unmap()
		if !e.isTrusted(a) {
			return a.String()
		}
	}
	return peer.String() // every hop was a trusted proxy
}
