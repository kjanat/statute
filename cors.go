package statute

import (
	"net/http"
	"slices"
	"strconv"
	"strings"

	"statute.kjanat.dev/resolved"
)

type corsMW struct {
	origins       []string
	methods       []string
	headers       []string
	exposeHeaders []string
	credentials   bool
	maxAge        string
}

func (*corsMW) statuteMiddleware() {}

// CORS returns a Cross-Origin Resource Sharing middleware. Pass at least
// Origins(...) to enable; without explicit origins the middleware does
// nothing.
//
// Preflight (OPTIONS + Access-Control-Request-Method) is handled directly
// and responds 204 with the configured headers. Non-preflight requests are
// passed through after Access-Control-Allow-* headers are set; the
// upstream handler still sees the request.
//
// A wildcard origin ("*") combined with Credentials() is rejected at resolve
// time — the CORS spec forbids credentialed wildcards.
func CORS() *corsMW {
	return &corsMW{
		methods: []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
	}
}

// Origins lists the allowed origins. Use "*" for any origin (no credentials).
// Multiple specific origins are matched exactly, case-insensitively per the
// CORS spec.
func (c *corsMW) Origins(origins ...string) *corsMW {
	c.origins = append(c.origins, origins...)
	return c
}

// Methods overrides the default allowed methods (GET/HEAD/POST/PUT/PATCH/DELETE/OPTIONS).
func (c *corsMW) Methods(methods ...string) *corsMW {
	c.methods = append(c.methods[:0], methods...)
	return c
}

// Headers lists the request headers the client may send. The browser sends
// these in Access-Control-Request-Headers during preflight; the middleware
// echoes them back in Access-Control-Allow-Headers when permitted.
func (c *corsMW) Headers(headers ...string) *corsMW {
	c.headers = append(c.headers, headers...)
	return c
}

// ExposeHeaders lists response headers the browser is allowed to surface to
// JavaScript. By default browsers expose only a small whitelist.
func (c *corsMW) ExposeHeaders(headers ...string) *corsMW {
	c.exposeHeaders = append(c.exposeHeaders, headers...)
	return c
}

// Credentials allows credentials (cookies, authorization headers, TLS client
// certs) on cross-origin requests. Requires specific origins (no wildcard).
func (c *corsMW) Credentials() *corsMW {
	c.credentials = true
	return c
}

// MaxAge caches preflight responses for the given duration on the client.
// Pass a duration string like "1h" or "10m".
func (c *corsMW) MaxAge(dur string) *corsMW {
	c.maxAge = dur
	return c
}

// corsHandler implements the CORS protocol.
func corsHandler(m resolved.Middleware, next http.Handler) http.Handler {
	allowAll := m.CORSAllowAllOrigin
	allowed := m.CORSOrigins
	methodsHeader := strings.Join(m.CORSMethods, ", ")
	headersHeader := strings.Join(m.CORSHeaders, ", ")
	exposeHeader := strings.Join(m.CORSExposeHeaders, ", ")
	credentials := m.CORSCredentials
	maxAge := ""
	if m.CORSMaxAge > 0 {
		maxAge = strconv.Itoa(int(m.CORSMaxAge.Seconds()))
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")

		// Vary: Origin is set unconditionally on responses from this handler
		// so caches do not serve the wrong CORS headers to a different origin.
		appendVary(w.Header(), "Origin")

		matched := allowAll || (origin != "" && slices.Contains(allowed, origin))
		if !matched && origin == "" {
			// Not a CORS request at all — pass through unchanged.
			next.ServeHTTP(w, r)
			return
		}

		if matched {
			setCORSResponseHeaders(w, origin, allowAll, credentials, exposeHeader)
		}

		// Preflight: OPTIONS with Access-Control-Request-Method short-circuits
		// after emitting the policy headers.
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			if matched {
				setCORSPreflightHeaders(w, r, methodsHeader, headersHeader, maxAge)
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// setCORSResponseHeaders writes the Access-Control-Allow-Origin and related
// headers that apply to every matched request (preflight or not).
func setCORSResponseHeaders(w http.ResponseWriter, origin string, allowAll, credentials bool, exposeHeader string) {
	if allowAll {
		w.Header().Set("Access-Control-Allow-Origin", "*")
	} else {
		w.Header().Set("Access-Control-Allow-Origin", origin)
	}
	if credentials {
		w.Header().Set("Access-Control-Allow-Credentials", "true")
	}
	if exposeHeader != "" {
		w.Header().Set("Access-Control-Expose-Headers", exposeHeader)
	}
}

// setCORSPreflightHeaders writes the preflight-only policy headers
// (allowed methods, allowed headers, max-age) for a matched OPTIONS request.
func setCORSPreflightHeaders(w http.ResponseWriter, r *http.Request, methodsHeader, headersHeader, maxAge string) {
	if methodsHeader != "" {
		w.Header().Set("Access-Control-Allow-Methods", methodsHeader)
	}
	reqHeaders := r.Header.Get("Access-Control-Request-Headers")
	if headersHeader != "" {
		w.Header().Set("Access-Control-Allow-Headers", headersHeader)
	} else if reqHeaders != "" {
		// Echo back what the client asked for.
		w.Header().Set("Access-Control-Allow-Headers", reqHeaders)
	}
	if maxAge != "" {
		w.Header().Set("Access-Control-Max-Age", maxAge)
	}
}

// appendVary appends a value to an existing Vary header, deduplicating.
func appendVary(h http.Header, value string) {
	current := h.Get("Vary")
	if current == "" {
		h.Set("Vary", value)
		return
	}
	for part := range strings.SplitSeq(current, ",") {
		if strings.EqualFold(strings.TrimSpace(part), value) {
			return
		}
	}
	h.Set("Vary", current+", "+value)
}
