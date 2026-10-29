package statute

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"

	"statute.kjanat.dev/resolved"
)

type ingressForwardedKey struct{}

type ingressForwardedValues struct {
	host, proto, chain string
}

var ingressForwardedHeaders = map[string]bool{
	headerXForwardedProto: true, headerXForwardedFor: true, headerXForwardedHost: true,
	headerXForwardedPort: true, "X-Forwarded-Server": true, "X-Forwarded-Uri": true,
	"X-Forwarded-Method": true, "X-Forwarded-Prefix": true, headerXRealIP: true,
	"X-Forwarded-Tls-Client-Cert": true, "X-Forwarded-Tls-Client-Cert-Info": true,
}

func ingressHandler(policy *resolved.IngressPolicy, next http.Handler) http.Handler {
	if policy == nil {
		return next
	}
	hostname, _ := os.Hostname()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status := ingressRequestStatus(r, policy); status != 0 {
			w.WriteHeader(status)
			return
		}
		if policy.ForwardedHeaders {
			ingressForwarded(r, hostname)
			values := ingressForwardedValues{
				host: r.Header.Get(headerXForwardedHost), proto: r.Header.Get(headerXForwardedProto),
				chain: strings.Join(r.Header.Values(headerXForwardedFor), ", "),
			}
			r = r.WithContext(context.WithValue(r.Context(), ingressForwardedKey{}, values))
		}
		if policy.NormalizePath || policy.SanitizePath {
			var err error
			r, err = ingressPath(r, policy)
			if err != nil {
				http.Error(w, "invalid request path", http.StatusBadRequest)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func ingressAliases(headers http.Header, policy HeaderAliasPolicy) bool {
	if policy == HeaderAliasesKeep {
		return true
	}
	underscores := policy == HeaderUnderscoresDelete || policy == HeaderUnderscoresReject
	reject := policy == HeaderAliasesReject || policy == HeaderUnderscoresReject
	for name := range headers {
		alias := strings.Contains(name, "_")
		if !underscores {
			alias = aliasHeaderName(name)
		}
		if !alias {
			continue
		}
		if reject {
			return false
		}
		delete(headers, name)
	}
	return true
}

func aliasHeaderName(name string) bool {
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c == '-' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			continue
		}
		return true
	}
	return false
}

func ingressForwarded(r *http.Request, hostname string) {
	peer, _ := netip.ParseAddrPort(r.RemoteAddr)
	sanitizeIngressForwarded(r, peer)
	if r.Header.Get(headerXRealIP) == "" && peer.IsValid() {
		r.Header.Set(headerXRealIP, peer.Addr().Unmap().WithZone("").String())
	}
	setMissingIngressHeader(r.Header, headerXForwardedProto, ingressScheme(r))
	setMissingIngressHeader(r.Header, headerXForwardedHost, r.Host)
	setMissingIngressHeader(r.Header, headerXForwardedPort, ingressPort(r))
	if values := r.Header.Values(headerXForwardedFor); len(values) > 0 {
		r.Header.Set(headerXForwardedFor, strings.Join(values, ", "))
	}
	if hostname != "" {
		r.Header.Set("X-Forwarded-Server", hostname)
	}
	ingressConnectionHeaders(r)
}

func sanitizeIngressForwarded(r *http.Request, peer netip.AddrPort) {
	policy := trustedProxyFromContext(r)
	if policy != nil && peer.IsValid() && addrInPrefixes(peer.Addr(), policy.prefixes) {
		return
	}
	for name := range r.Header {
		canonical := http.CanonicalHeaderKey(strings.ReplaceAll(name, "_", "-"))
		if ingressForwardedHeaders[canonical] {
			delete(r.Header, name)
		}
	}
}

func ingressScheme(r *http.Request) string {
	secure := r.TLS != nil
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") && headerHasToken(r.Header.Values("Connection"), "upgrade") {
		if secure {
			return "wss"
		}
		return "ws"
	}
	if secure {
		return schemeHTTPS
	}
	return schemeHTTP
}

func headerHasToken(values []string, token string) bool {
	for _, value := range values {
		for part := range strings.SplitSeq(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

func ingressPort(r *http.Request) string {
	if _, port, err := net.SplitHostPort(r.Host); err == nil && port != "" {
		return port
	}
	proto := r.Header.Get(headerXForwardedProto)
	if r.TLS != nil || proto == schemeHTTPS || proto == "wss" {
		return "443"
	}
	return "80"
}

func ingressConnectionHeaders(r *http.Request) {
	upgrade := ""
	for _, value := range r.Header.Values("Connection") {
		for token := range strings.SplitSeq(value, ",") {
			name := http.CanonicalHeaderKey(strings.TrimSpace(token))
			if strings.EqualFold(name, "Upgrade") {
				upgrade = r.Header.Get("Upgrade")
			}
			if !ingressForwardedHeaders[http.CanonicalHeaderKey(strings.ReplaceAll(name, "_", "-"))] {
				r.Header.Del(name)
			}
		}
	}
	r.Header.Del("Connection")
	if upgrade != "" {
		r.Header.Set("Connection", "Upgrade")
		r.Header.Set("Upgrade", upgrade)
	}
}

func applyIngressForwarded(in *http.Request, out http.Header) {
	values, enabled := in.Context().Value(ingressForwardedKey{}).(ingressForwardedValues)
	if !enabled {
		return
	}
	out.Set(headerXForwardedHost, values.host)
	out.Set(headerXForwardedProto, values.proto)
	if values.chain != "" {
		chain := values.chain
		if host, _, err := net.SplitHostPort(in.RemoteAddr); err == nil {
			chain += ", " + host
		}
		out.Set(headerXForwardedFor, chain)
	}
}

func setMissingIngressHeader(h http.Header, name, value string) {
	if h.Get(name) == "" {
		h.Set(name, value)
	}
}

func ingressPath(r *http.Request, p *resolved.IngressPolicy) (*http.Request, error) {
	out := r.Clone(r.Context())
	out.Trailer = r.Trailer
	changed := p.SanitizePath
	if p.NormalizePath && out.URL.RawPath != "" {
		raw, err := normalizeIngressEscapes(out.URL.RawPath)
		if err != nil {
			return nil, err
		}
		changed = changed || raw != out.URL.RawPath
		out.URL.RawPath = raw
	}
	if p.SanitizePath {
		out.URL = out.URL.JoinPath()
	}
	if changed {
		out.RequestURI = out.URL.RequestURI()
	}
	return out, nil
}

func normalizeIngressEscapes(raw string) (string, error) {
	var normalized strings.Builder
	for i := 0; i < len(raw); i++ {
		if raw[i] != '%' {
			normalized.WriteByte(raw[i])
			continue
		}
		if i+2 >= len(raw) {
			return "", url.InvalidHostError("invalid escape")
		}
		encoded := strings.ToUpper(raw[i : i+3])
		value, err := strconv.ParseUint(encoded[1:], 16, 8)
		if err != nil {
			return "", err
		}
		c := byte(value)
		if ingressUnreserved(c) {
			normalized.WriteByte(c)
		} else {
			normalized.WriteString(encoded)
		}
		i += 2
	}
	return normalized.String(), nil
}

func ingressUnreserved(c byte) bool {
	return !aliasHeaderName(string(c)) || c == '.' || c == '_' || c == '~'
}

func ingressRequestStatus(r *http.Request, p *resolved.IngressPolicy) int {
	if p.NormalizePath && r.URL.Opaque != "" {
		return http.StatusBadRequest
	}
	if !ingressAliases(r.Header, p.HeaderAliases) {
		return http.StatusBadRequest
	}
	if !p.NormalizePath {
		return 0
	}
	if r.Method == http.MethodConnect {
		return http.StatusNotImplemented
	}
	if strings.Contains(r.URL.RawPath, "#") {
		return http.StatusBadRequest
	}
	return 0
}
