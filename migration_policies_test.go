package statute

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"

	"statute.kjanat.dev/resolved"
)

func TestHTTPSHeaderOperationAtCommit(t *testing.T) {
	m, err := resolveHeaderMW(SetResponseHeader("Strict-Transport-Security", "max-age=31536000; includeSubDomains; preload").OnHTTPS())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolveHeaderMW(SetRequestHeader("X-Auth", "value").OnHTTPS()); err == nil {
		t.Fatal("request header accepted HTTPS response option")
	}
	h := withHeaderMiddleware([]resolved.Middleware{m}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Strict-Transport-Security", "upstream")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	for _, secure := range []bool{false, true} {
		r := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
		if secure {
			r.TLS = &tls.ConnectionState{}
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		want := "upstream"
		if secure {
			want = m.HeaderValue
		}
		if rec.Code != http.StatusUnauthorized || rec.Header().Get(m.HeaderName) != want {
			t.Fatalf("secure=%t status=%d header=%q", secure, rec.Code, rec.Header().Get(m.HeaderName))
		}
	}
}

func TestRedirectNonGETPolicy(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, "CUSTOM"} {
		r := httptest.NewRequest(method, "http://example.test/a?x=1", nil)
		want := http.StatusPermanentRedirect
		if method == http.MethodGet {
			want = http.StatusMovedPermanently
		}
		for _, h := range []http.Handler{
			redirectRouteHandler(&resolved.Redirect{Target: "https://example.test{request_uri}", Status: 301, NonGETStatus: 308}),
			redirectHandlerWithMethods("https", 308),
		} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if rec.Code != want || rec.Header().Get("Location") != "https://example.test/a?x=1" {
				t.Fatalf("method=%s status=%d target=%s", method, rec.Code, rec.Header().Get("Location"))
			}
		}
	}
	r := httptest.NewRequest(http.MethodPost, "http://example.test/", nil)
	rec := httptest.NewRecorder()
	redirectHandler("https").ServeHTTP(rec, r)
	if rec.Code != http.StatusMovedPermanently {
		t.Fatal("existing redirect default changed")
	}
	if _, err := resolveListener(HTTP(":8080").RedirectNonGET(308)); err == nil {
		t.Fatal("non-redirect listener accepted redirect policy")
	}
	if _, err := resolveListener(HTTP(":8080").RedirectTo("https").RedirectNonGET(200)); err == nil {
		t.Fatal("invalid redirect status accepted")
	}
}

func TestHTTPSHeadersStayRequestScopedAcrossCache(t *testing.T) {
	calls := 0
	h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Strict-Transport-Security", "upstream")
		w.WriteHeader(http.StatusOK)
	}),
		SetResponseHeader("Strict-Transport-Security", "max-age=31536000").OnHTTPS(),
		SetResponseHeader("Cache-Control", "no-store").OnHTTPS(),
		Cache("1m"),
	)
	for _, secure := range []bool{false, true, false, true, false} {
		r := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
		if secure {
			r.TLS = &tls.ConnectionState{}
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		want, cacheControl := "upstream", ""
		if secure {
			want, cacheControl = "max-age=31536000", "no-store"
		}
		if rec.Header().Get("Strict-Transport-Security") != want || rec.Header().Get("Cache-Control") != cacheControl {
			t.Fatalf("TLS=%t headers=%v", secure, rec.Header())
		}
	}
	if calls != 3 {
		t.Fatalf("origin calls=%d, want one HTTP miss and two uncacheable HTTPS requests", calls)
	}
}
