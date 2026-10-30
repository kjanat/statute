package statute

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"statute.kjanat.dev/resolved"
)

type redirectMW struct {
	pattern, replacement string
	scheme, port         string
	status, nonGETStatus int
	schemeRedirect       bool
}

func (*redirectMW) statuteMiddleware() {}

// RedirectRegex matches the full request URL and expands captures using Go's
// regexp replacement syntax. A non-match or unchanged URL calls the next handler.
// Defaults are 302 for GET and 307 for other methods, including HEAD.
func RedirectRegex(pattern, replacement string) *redirectMW {
	return &redirectMW{pattern: pattern, replacement: replacement, status: http.StatusFound, nonGETStatus: http.StatusTemporaryRedirect}
}

// RedirectScheme redirects to scheme while keeping the host, path and query.
// Forwarded schemes are accepted only after listener ingress sanitation or from
// a trusted proxy. Requests already at the target URL call the next handler.
func RedirectScheme(scheme string) *redirectMW {
	return &redirectMW{scheme: scheme, schemeRedirect: true, status: http.StatusFound, nonGETStatus: http.StatusTemporaryRedirect}
}

// Port selects the target port for RedirectScheme. Standard ports are omitted.
func (m *redirectMW) Port(port string) *redirectMW { m.port = port; return m }

// Permanent selects 301 for GET and 308 for every other method.
func (m *redirectMW) Permanent() *redirectMW {
	m.status, m.nonGETStatus = http.StatusMovedPermanently, http.StatusPermanentRedirect
	return m
}

// Status selects the same redirect status for every method.
func (m *redirectMW) Status(status int) *redirectMW {
	m.status, m.nonGETStatus = status, status
	return m
}

func (m *redirectMW) resolve() (resolved.Middleware, error) {
	if !redirectMiddlewareStatus(m.status) || !redirectMiddlewareStatus(m.nonGETStatus) {
		return resolved.Middleware{}, errors.New("redirect: status must be 301, 302, 303, 307 or 308")
	}
	out := resolved.Middleware{Type: resolved.MWRedirectRegex, RedirectPattern: m.pattern, RedirectReplacement: m.replacement, RedirectStatus: m.status, RedirectNonGETStatus: m.nonGETStatus}
	if m.schemeRedirect {
		if err := validateRedirectScheme(m.scheme, m.port); err != nil {
			return resolved.Middleware{}, err
		}
		out.Type, out.RedirectScheme, out.RedirectPort = resolved.MWRedirectScheme, m.scheme, m.port
		return out, nil
	}
	if m.port != "" {
		return resolved.Middleware{}, errors.New("redirect_regex: Port requires RedirectScheme")
	}
	if _, err := regexp.Compile(m.pattern); err != nil {
		return resolved.Middleware{}, fmt.Errorf("redirect_regex: %w", err)
	}
	return out, nil
}

func redirectMiddlewareStatus(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	default:
		return false
	}
}

var redirectAbsoluteURI = regexp.MustCompile(`^(https?)://(\[[\w:.]+\]|[\w._-]+)?(:\d+)?(.*)$`)

const redirectSchemePattern = `^(https?://)?(\[[\w:.]+\]|[\w._-]+)?(:\d+)?(.*)$`

func redirectMiddlewareHandler(m resolved.Middleware, next http.Handler) http.Handler {
	pattern, replacement := m.RedirectPattern, m.RedirectReplacement
	if m.Type == resolved.MWRedirectScheme {
		port := redirectTargetPort(m.RedirectScheme, m.RedirectPort)
		pattern, replacement = redirectSchemePattern, m.RedirectScheme+"://${2}"+port+"${4}"
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		// Invalid patterns refuse requests, including hand-built resolved values.
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeRedirectStatus(w, http.StatusInternalServerError) })
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oldURL := redirectRequestURL(r, m.Type == resolved.MWRedirectScheme)
		if !re.MatchString(oldURL) {
			next.ServeHTTP(w, r)
			return
		}
		target := re.ReplaceAllString(oldURL, replacement)
		parsed, err := url.Parse(target)
		if err != nil {
			writeRedirectStatus(w, http.StatusInternalServerError)
			return
		}
		if target == oldURL {
			out := new(http.Request)
			*out = *r
			out.URL, out.RequestURI = parsed, parsed.RequestURI()
			next.ServeHTTP(w, out)
			return
		}
		status := m.RedirectStatus
		if r.Method != http.MethodGet {
			status = m.RedirectNonGETStatus
		}
		w.Header().Set("Location", parsed.String())
		writeRedirectStatus(w, status)
	})
}

func writeRedirectStatus(w http.ResponseWriter, status int) {
	w.WriteHeader(status)
	_, _ = w.Write([]byte(http.StatusText(status)))
}

func redirectRequestURL(r *http.Request, forwarded bool) string {
	scheme, host, port, uri := schemeHTTP, r.Host, "", r.RequestURI
	if uri == "" && r.URL != nil {
		uri = r.URL.RequestURI()
	}
	if forwarded {
		if name, number, err := net.SplitHostPort(host); err == nil {
			host, port = name, ":"+number
		}
	}
	if match := redirectAbsoluteURI.FindStringSubmatch(uri); match != nil {
		scheme, uri = match[1], match[4]
		if match[2] != "" {
			host = match[2]
		}
		if match[3] != "" {
			port = match[3]
		}
	}
	if r.TLS != nil {
		scheme = schemeHTTPS
	}
	if forwarded {
		scheme, port = forwardedRedirectAuthority(r, scheme, port)
	}
	return scheme + "://" + host + port + uri
}

func redirectForwardedProto(r *http.Request) string {
	if _, sanitized := r.Context().Value(ingressForwardedKey{}).(ingressForwardedValues); sanitized {
		return r.Header.Get(headerXForwardedProto)
	}
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if policy := trustedProxyFromContext(r); err == nil && policy != nil && addrInPrefixes(peer.Addr(), policy.prefixes) {
		return r.Header.Get(headerXForwardedProto)
	}
	return ""
}

func validateRedirectScheme(scheme, port string) error {
	if scheme != schemeHTTP && scheme != schemeHTTPS {
		return errors.New("redirect_scheme: scheme must be http or https")
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("redirect_scheme: port must be between 1 and 65535")
		}
	}
	return nil
}

func forwardedRedirectAuthority(r *http.Request, scheme, port string) (string, string) {
	switch proto := redirectForwardedProto(r); {
	case strings.EqualFold(proto, schemeHTTP), strings.EqualFold(proto, "ws"):
		scheme = schemeHTTP
	case strings.EqualFold(proto, schemeHTTPS), strings.EqualFold(proto, "wss"):
		scheme = schemeHTTPS
	}
	if redirectStandardPort(scheme, strings.TrimPrefix(port, ":")) {
		port = ""
	}
	return scheme, port
}

func redirectTargetPort(scheme, port string) string {
	if port == "" || redirectStandardPort(scheme, port) {
		return ""
	}
	return ":" + port
}

func redirectStandardPort(scheme, port string) bool {
	return scheme == schemeHTTP && port == "80" || scheme == schemeHTTPS && port == "443"
}
