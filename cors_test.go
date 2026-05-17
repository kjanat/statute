package statute

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCORS_PreflightAllowed(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("preflight must short-circuit; upstream invoked")
		w.WriteHeader(http.StatusOK)
	})
	h := corsHandler(mustResolveMW(t, CORS().Origins("https://app.example.com").MaxAge("1h")), inner)

	req := httptest.NewRequest("OPTIONS", "/anything", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "Authorization, Content-Type")
	rec := runRequest(t, h, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: got %d, want 204", rec.Code)
	}
	assertHeader(t, rec.Header(), "Access-Control-Allow-Origin", "https://app.example.com")
	assertHeader(t, rec.Header(), "Access-Control-Allow-Headers", "Authorization, Content-Type")
	assertHeader(t, rec.Header(), "Access-Control-Max-Age", "3600")
	if vary := rec.Header().Get("Vary"); !strings.Contains(vary, "Origin") {
		t.Errorf("Vary: got %q, want to contain Origin", vary)
	}
}

func TestCORS_OriginDisallowed(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := corsHandler(mustResolveMW(t, CORS().Origins("https://app.example.com")), inner)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec := runRequest(t, h, req)

	// Upstream runs (we don't block), but no Allow-Origin is set.
	if rec.Code != http.StatusOK {
		t.Errorf("status: %d", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("disallowed origin must not get ACAO header")
	}
}

func TestCORS_WildcardOrigin(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := corsHandler(mustResolveMW(t, CORS().Origins("*")), inner)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Origin", "https://anything.example.com")
	rec := runRequest(t, h, req)

	assertHeader(t, rec.Header(), "Access-Control-Allow-Origin", "*")
}

func TestCORS_CredentialsWithWildcardRejected(t *testing.T) {
	t.Parallel()
	_, err := resolveMiddleware(CORS().Origins("*").Credentials())
	if err == nil {
		t.Fatal("want error for wildcard + credentials per spec")
	}
}

func TestCORS_NoOriginsConfiguredErrors(t *testing.T) {
	t.Parallel()
	_, err := resolveMiddleware(CORS())
	if err == nil {
		t.Fatal("want error when no Origins configured")
	}
}

func TestCORS_SimpleRequestEmitsHeaders(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := corsHandler(
		mustResolveMW(t, CORS().Origins("https://app.example.com").Credentials().ExposeHeaders("X-Total-Count")),
		inner,
	)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Origin", "https://app.example.com")
	rec := runRequest(t, h, req)

	assertHeader(t, rec.Header(), "Access-Control-Allow-Origin", "https://app.example.com")
	assertHeader(t, rec.Header(), "Access-Control-Allow-Credentials", "true")
	assertHeader(t, rec.Header(), "Access-Control-Expose-Headers", "X-Total-Count")
}

func TestCORS_VaryAlwaysSet(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})
	h := corsHandler(mustResolveMW(t, CORS().Origins("https://app.example.com")), inner)

	// Request without Origin header — Vary must still be set.
	rec := runRequest(t, h, httptest.NewRequest("GET", "/", nil))
	if !strings.Contains(rec.Header().Get("Vary"), "Origin") {
		t.Errorf("Vary: got %q", rec.Header().Get("Vary"))
	}
}
