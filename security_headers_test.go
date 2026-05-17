package statute

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kjanat/statute/resolved"
)

func TestSecurityHeaders_Defaults(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := securityHeadersHandler(mustResolveMW(t, SecurityHeaders()), inner)
	rec := runRequest(t, h, httptest.NewRequest("GET", "/", nil))

	assertHeader(t, rec.Header(), "X-Content-Type-Options", "nosniff")
	assertHeader(t, rec.Header(), "X-Frame-Options", "DENY")
	assertHeader(t, rec.Header(), "Referrer-Policy", "strict-origin-when-cross-origin")
	assertNoHeader(t, rec.Header(), "Strict-Transport-Security")
	assertNoHeader(t, rec.Header(), "Content-Security-Policy")
}

func TestSecurityHeaders_HSTSOptIn(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := securityHeadersHandler(mustResolveMW(t, SecurityHeaders().HSTS("365d")), inner)
	rec := runRequest(t, h, httptest.NewRequest("GET", "/", nil))

	got := rec.Header().Get("Strict-Transport-Security")
	if !strings.HasPrefix(got, "max-age=") {
		t.Errorf("HSTS: got %q", got)
	}
	if !strings.Contains(got, "includeSubDomains") {
		t.Errorf("HSTS missing includeSubDomains: %q", got)
	}
}

func TestSecurityHeaders_CSPPassthrough(t *testing.T) {
	t.Parallel()
	csp := "default-src 'self'; script-src 'self' 'unsafe-inline'"
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})
	h := securityHeadersHandler(mustResolveMW(t, SecurityHeaders().CSP(csp)), inner)
	rec := runRequest(t, h, httptest.NewRequest("GET", "/", nil))
	if rec.Header().Get("Content-Security-Policy") != csp {
		t.Errorf("CSP not passthrough")
	}
}

func TestSecurityHeaders_OverrideAndSuppress(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})
	h := securityHeadersHandler(
		mustResolveMW(t, SecurityHeaders().
			FrameOptions("SAMEORIGIN").
			ContentTypeOptions(false).
			ReferrerPolicy("")),
		inner,
	)
	rec := runRequest(t, h, httptest.NewRequest("GET", "/", nil))
	assertHeader(t, rec.Header(), "X-Frame-Options", "SAMEORIGIN")
	assertNoHeader(t, rec.Header(), "X-Content-Type-Options")
	assertNoHeader(t, rec.Header(), "Referrer-Policy")
}

func TestSecurityHeaders_HSTSInvalidDurationErrors(t *testing.T) {
	t.Parallel()
	_, err := resolveMiddleware(SecurityHeaders().HSTS("forever"))
	if err == nil {
		t.Fatal("want error for invalid HSTS duration")
	}
}

// mustResolveMW resolves one surface middleware via the package's
// resolveMiddleware and fails the test on error.
func mustResolveMW(t *testing.T, mw Middleware) resolved.Middleware {
	t.Helper()
	r, err := resolveMiddleware(mw)
	if err != nil {
		t.Fatalf("resolveMiddleware: %v", err)
	}
	return r
}
