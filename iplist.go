package statute

import (
	"net/http"
	"net/netip"

	"statute.kjanat.dev/resolved"
)

type allowIPsMW struct{ cidrs []string }

func (*allowIPsMW) statuteMiddleware() {}

// AllowIPs returns a middleware that admits only requests whose client IP
// falls within at least one of the configured CIDR ranges. Other requests
// are answered with 403 Forbidden.
//
// CIDRs are parsed at resolve time via net/netip; both IPv4 and IPv6 are
// supported. Examples: "10.0.0.0/8", "2001:db8::/32", "203.0.113.5/32".
//
// The client IP comes from clientIP(), which respects the listener's
// TrustedProxy() policy and BehindCloudflare() (CF-Connecting-IP) when
// configured.
//
// AllowIPs answers 403 and stops; to fall through to a later route for
// outside clients instead, constrain the route itself with Route.ClientIPs.
func AllowIPs(cidrs ...string) *allowIPsMW {
	return &allowIPsMW{cidrs: cidrs}
}

type denyIPsMW struct{ cidrs []string }

func (*denyIPsMW) statuteMiddleware() {}

// DenyIPs returns a middleware that rejects requests whose client IP falls
// within at least one of the configured CIDR ranges. Other requests pass
// through.
//
// DenyIPs is checked independently of AllowIPs; if both are configured on
// a route, the order they were added to With(...) determines precedence.
func DenyIPs(cidrs ...string) *denyIPsMW {
	return &denyIPsMW{cidrs: cidrs}
}

// allowIPsHandler enforces the allow list. Requests from unmatched IPs are
// rejected with 403.
func allowIPsHandler(m resolved.Middleware, next http.Handler) http.Handler {
	prefixes := mustParsePrefixes(m.IPCIDRs)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ipInList(r, prefixes) {
			http.Error(w, "403 Forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// denyIPsHandler enforces the deny list. Requests from matched IPs are
// rejected with 403.
func denyIPsHandler(m resolved.Middleware, next http.Handler) http.Handler {
	prefixes := mustParsePrefixes(m.IPCIDRs)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ipInList(r, prefixes) {
			http.Error(w, "403 Forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ipInList returns true when the request's client IP falls within any of the
// listed prefixes. An unparseable IP (e.g. r.RemoteAddr in an unexpected
// form) is treated as no-match.
func ipInList(r *http.Request, prefixes []netip.Prefix) bool {
	addr, ok := parseClientAddr(r)
	if !ok {
		return false
	}
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// parseClientAddr extracts a netip.Addr from r using clientIP(), which
// already handles BehindCloudflare and X-Forwarded-For.
func parseClientAddr(r *http.Request) (netip.Addr, bool) {
	ip := clientIP(r)
	// Strip port if present (RemoteAddr is host:port).
	if h, _, err := splitHostPortLast(ip); err == nil {
		ip = h
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr, true
}

// mustParsePrefixes parses the resolved CIDR list. Resolve has already
// validated every entry, so a parse failure here is a logic error and we
// fall back to no-match (the safe default for allow rejects everything;
// the safe default for deny rejects nothing).
func mustParsePrefixes(cidrs []string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		if p, err := netip.ParsePrefix(c); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// splitHostPortLast is a lenient host:port splitter that tolerates IPv6
// addresses wrapped in brackets and addresses without a port.
func splitHostPortLast(s string) (host, port string, err error) {
	if len(s) == 0 {
		return "", "", errEmpty
	}
	// IPv6 "[host]:port" form
	if s[0] == '[' {
		end := indexByte(s, ']')
		if end < 0 {
			return "", "", errBadBracket
		}
		host = s[1:end]
		rest := s[end+1:]
		if len(rest) > 1 && rest[0] == ':' {
			port = rest[1:]
		}
		return host, port, nil
	}
	// IPv4 or bare host
	idx := indexByte(s, ':')
	if idx < 0 {
		return s, "", nil
	}
	// If multiple colons, it's an unbracketed IPv6 — treat the whole thing as host.
	if next := indexByteFrom(s, ':', idx+1); next >= 0 {
		return s, "", nil
	}
	return s[:idx], s[idx+1:], nil
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func indexByteFrom(s string, c byte, from int) int {
	for i := from; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

var (
	errEmpty      = errIPListLookup("address is empty")
	errBadBracket = errIPListLookup("unmatched bracket in address")
)

type errIPListLookup string

// Error implements error; the underlying string is the message.
func (e errIPListLookup) Error() string { return string(e) }
