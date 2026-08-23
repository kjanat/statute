package statute

import (
	"context"
	"net/http"
	"net/netip"
	"strings"

	"statute.kjanat.dev/resolved"
)

// trustedProxyPolicy is a listener's compiled peer-trust configuration:
// which direct peers may assert the real client IP, and through which
// forwarded header.
type trustedProxyPolicy struct {
	prefixes []netip.Prefix
	header   string
}

type tpCtxKey struct{}

// trustedProxyMiddleware compiles the listener's trusted ranges once and
// tags every request context with the policy, so clientIP — wherever it is
// consulted (access log, rate limit, IP lists, IP-hash picking) — resolves
// the client under per-peer trust instead of trusting headers blindly.
func trustedProxyMiddleware(l *resolved.Listener, next http.Handler) http.Handler {
	policy := &trustedProxyPolicy{
		prefixes: mustParsePrefixes(l.TrustedProxies),
		header:   l.ClientIPHeader,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), tpCtxKey{}, policy)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// trustedProxyFromContext returns the listener's peer-trust policy, or nil
// when the listener declared none.
func trustedProxyFromContext(r *http.Request) *trustedProxyPolicy {
	p, _ := r.Context().Value(tpCtxKey{}).(*trustedProxyPolicy)
	return p
}

// clientIP resolves the client under this policy. A peer inside the trusted
// ranges speaks for its client through the forwarded header; any other peer
// is its own client, and whatever forwarded headers it sent are ignored —
// that is what lets proxied and direct traffic share a listener safely.
//
// Of a multi-valued header, the last value counts: with one layer of
// trusted proxies it is the address the proxy itself observed, while the
// earlier values arrived from outside and remain client-controlled.
func (p *trustedProxyPolicy) clientIP(r *http.Request) string {
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil || !addrInPrefixes(peer.Addr(), p.prefixes) {
		return r.RemoteAddr
	}
	vals := r.Header.Values(p.header)
	if len(vals) == 0 {
		return r.RemoteAddr
	}
	last := vals[len(vals)-1]
	if i := strings.LastIndexByte(last, ','); i >= 0 {
		last = last[i+1:]
	}
	if trimmed := strings.TrimSpace(last); trimmed != "" {
		return trimmed
	}
	return r.RemoteAddr
}

// addrInPrefixes reports whether addr falls inside any of the prefixes.
// IPv4-mapped IPv6 peers are unmapped first, so "::ffff:203.0.113.7"
// matches an IPv4 range.
func addrInPrefixes(addr netip.Addr, prefixes []netip.Prefix) bool {
	addr = addr.Unmap()
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
