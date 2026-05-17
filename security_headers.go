package statute

import (
	"net/http"
	"strconv"
	"time"

	"github.com/kjanat/statute/resolved"
)

type securityHeadersMW struct {
	hsts               string
	csp                string
	frameOptions       string
	contentTypeOptions bool
	referrerPolicy     string
	permissionsPolicy  string
}

func (*securityHeadersMW) statuteMiddleware() {}

// SecurityHeaders returns a middleware that emits common HTTP security
// response headers. Defaults are conservative for a public-facing edge
// proxy:
//
//   - X-Content-Type-Options: nosniff
//   - X-Frame-Options: DENY
//   - Referrer-Policy: strict-origin-when-cross-origin
//
// HSTS, CSP, and Permissions-Policy are strictly opt-in via the builder
// methods. Override or disable an individual header by passing "" to its
// setter (or by not calling the setter, for HSTS/CSP/Permissions-Policy).
//
// The CSP string is passed through verbatim — no parsing, no validation.
// The framework cannot know what your application needs.
func SecurityHeaders() *securityHeadersMW {
	return &securityHeadersMW{
		contentTypeOptions: true,
		frameOptions:       "DENY",
		referrerPolicy:     "strict-origin-when-cross-origin",
	}
}

// HSTS sets the Strict-Transport-Security max-age. Pass a duration string
// like "365d" or "180d". Calling HSTS implies includeSubDomains; preload
// is not automatically enabled (you must submit your domain manually).
//
// HSTS on a development server bricks "localhost" for the configured age
// in browsers that have already received the header. Production-only.
func (s *securityHeadersMW) HSTS(maxAge string) *securityHeadersMW {
	s.hsts = maxAge
	return s
}

// CSP sets the Content-Security-Policy header. The value is sent verbatim;
// statute does not parse or validate it. Refer to MDN's CSP reference for
// the directive syntax.
func (s *securityHeadersMW) CSP(value string) *securityHeadersMW {
	s.csp = value
	return s
}

// FrameOptions sets the X-Frame-Options header. Common values: "DENY",
// "SAMEORIGIN". Pass "" to suppress the default ("DENY").
func (s *securityHeadersMW) FrameOptions(value string) *securityHeadersMW {
	s.frameOptions = value
	return s
}

// ContentTypeOptions toggles the X-Content-Type-Options: nosniff header.
// Defaults to true. Setting to false suppresses the header.
func (s *securityHeadersMW) ContentTypeOptions(enabled bool) *securityHeadersMW {
	s.contentTypeOptions = enabled
	return s
}

// ReferrerPolicy sets the Referrer-Policy header.
func (s *securityHeadersMW) ReferrerPolicy(value string) *securityHeadersMW {
	s.referrerPolicy = value
	return s
}

// PermissionsPolicy sets the Permissions-Policy header. Like CSP, the value
// is sent verbatim.
func (s *securityHeadersMW) PermissionsPolicy(value string) *securityHeadersMW {
	s.permissionsPolicy = value
	return s
}

// securityHeadersHandler emits the configured headers on every response. It
// sets headers before invoking the next handler so an upstream handler that
// commits its own response cannot accidentally clobber them — and so the
// headers are present even on error responses.
func securityHeadersHandler(m resolved.Middleware, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		if m.SecContentTypeOptions {
			h.Set("X-Content-Type-Options", "nosniff")
		}
		if m.SecFrameOptions != "" {
			h.Set("X-Frame-Options", m.SecFrameOptions)
		}
		if m.SecReferrerPolicy != "" {
			h.Set("Referrer-Policy", m.SecReferrerPolicy)
		}
		if m.SecHSTS != "" {
			h.Set("Strict-Transport-Security", m.SecHSTS)
		}
		if m.SecCSP != "" {
			h.Set("Content-Security-Policy", m.SecCSP)
		}
		if m.SecPermissionsPolicy != "" {
			h.Set("Permissions-Policy", m.SecPermissionsPolicy)
		}
		next.ServeHTTP(w, r)
	})
}

// formatHSTS turns a duration into the canonical "max-age=N; includeSubDomains"
// form. Used by resolve.
func formatHSTS(maxAge time.Duration) string {
	if maxAge <= 0 {
		return ""
	}
	seconds := int64(maxAge / time.Second)
	return "max-age=" + strconv.FormatInt(seconds, 10) + "; includeSubDomains"
}
