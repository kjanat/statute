package statute

import (
	"bytes"
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"statute.kjanat.dev/resolved"
)

func TestConditionalRedirectFallthroughAndRetry(t *testing.T) {
	for _, tc := range []struct {
		name, pattern, replacement, path string
		wantCalls                        int
		wantLocation                     string
	}{
		{"nonmatch", `^http://example.test/old/(.*)$`, "https://new.test/${1}", "/unchanged?x=a%2Fb", 2, ""},
		{"equal", `^(.*)$`, "${1}", "/unchanged?x=a%2Fb", 2, ""},
		{"capture", `^http://example.test/old/([^/]+)/(.*)$`, "https://new.test/${2}/${1}", "/old/first/second?x=a%2Fb", 0, "https://new.test/second?x=a%2Fb/first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.RequestURI() != tc.path || r.RequestURI != tc.path {
					t.Errorf("fallthrough changed target: URL=%s URI=%s", r.URL, r.RequestURI)
				}
				w.WriteHeader(http.StatusBadGateway)
			}), Retry(2, OnStatus(http.StatusBadGateway)), RedirectRegex(tc.pattern, tc.replacement).Permanent())
			r := httptest.NewRequest(http.MethodGet, "http://example.test"+tc.path, nil)
			r.RequestURI = tc.path
			originalURL := r.URL.String()
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if calls != tc.wantCalls || w.Header().Get("Location") != tc.wantLocation {
				t.Fatalf("calls=%d headers=%v", calls, w.Header())
			}
			if r.URL.String() != originalURL || r.RequestURI != tc.path {
				t.Fatal("redirect changed outer request")
			}
		})
	}
}

func TestConditionalRedirectSchemeTrust(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	policy := &trustedProxyPolicy{prefixes: mustParsePrefixes([]string{"192.0.2.0/24"}), header: headerXForwardedFor}
	for _, tc := range []struct {
		name, peer, proto string
		trusted, ingress  bool
		secure            bool
		want              int
	}{
		{"untrusted", "198.51.100.7:1234", "https", true, false, false, 302},
		{"no-policy", "192.0.2.7:1234", "https", false, false, false, 302},
		{"trusted", "192.0.2.7:1234", "https", true, false, false, 204},
		{"websocket", "192.0.2.7:1234", "WSS", true, false, false, 204},
		{"invalid-proto", "192.0.2.7:1234", "invalid", true, false, false, 302},
		{"tls", "198.51.100.7:1234", "", false, false, true, 204},
		{"sanitized-untrusted", "198.51.100.7:1234", "https", true, true, false, 302},
		{"sanitized-trusted", "192.0.2.7:1234", "https", true, true, false, 204},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://example.test/a?x=a%2Fb", nil)
			r.RequestURI = "/a?x=a%2Fb"
			r.RemoteAddr = tc.peer
			r.Header.Set(headerXForwardedProto, tc.proto)
			if tc.secure {
				r.TLS = &tls.ConnectionState{}
			}
			if tc.trusted {
				r = r.WithContext(context.WithValue(r.Context(), tpCtxKey{}, policy))
			}
			h := chain(t, base, RedirectScheme("https"))
			if tc.ingress {
				h = ingressHandler(&resolved.IngressPolicy{ForwardedHeaders: true}, h)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status=%d headers=%v", w.Code, w.Header())
			}
			if tc.want == 302 && w.Header().Get("Location") != "https://example.test/a?x=a%2Fb" {
				t.Fatal(w.Header())
			}
		})
	}
}

func TestConditionalRedirectResolutionAndTooling(t *testing.T) {
	for _, mw := range []Middleware{
		RedirectRegex("[", "target"), RedirectRegex(".*", "target").Status(200), RedirectRegex(".*", "target").Port("80"),
		RedirectScheme("ftp"), RedirectScheme("https").Port("0"), RedirectScheme("https").Port("65536"), RedirectScheme("https").Port("bad"),
	} {
		if _, err := resolveMiddleware(mw); err == nil {
			t.Fatalf("accepted invalid declaration: %+v", mw)
		}
	}
	cfg := Config{Listeners: Listeners{HTTP(":8080")}, Routes: Routes{
		Match("/*").Handle(http.NotFoundHandler()).With(RedirectRegex(`^http://example.test/(.*)`, "https://new.test/${1}").Permanent(), RedirectScheme("https").Port("8443").Status(308)),
	}}
	var export, graph bytes.Buffer
	if err := Export(cfg, &export); err != nil {
		t.Fatal(err)
	}
	if err := GraphDOT(cfg, &graph); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{export.String(), graph.String()} {
		if !strings.Contains(text, "https://new.test/${1}") || !strings.Contains(text, "8443") {
			t.Fatalf("missing redirect policy: %s", text)
		}
	}
}

func TestConditionalRedirectInvalidTargetDoesNotServe(t *testing.T) {
	h := chain(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("invalid redirect fell through") }), RedirectRegex(".*", "%invalid"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusInternalServerError || w.Header().Get("Location") != "" {
		t.Fatalf("invalid target: %d %v", w.Code, w.Header())
	}
	w = httptest.NewRecorder()
	redirectMiddlewareHandler(resolved.Middleware{Type: resolved.MWRedirectRegex, RedirectPattern: "["}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("invalid policy fell through") })).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatal(w.Code)
	}
}
