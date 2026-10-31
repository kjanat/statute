package statute

import (
	"bytes"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"statute.kjanat.dev/resolved"
)

func TestIngressProtocolAndHopHeaderSanitation(t *testing.T) {
	for _, tc := range []struct {
		name, authority, wantProto, wantPort string
		tls, upgrade                         bool
	}{
		{"http", "example.test", "http", "80", false, false},
		{"https", "example.test", "https", "443", true, false},
		{"explicit_port", "example.test:8080", "http", "8080", false, false},
		{"websocket", "example.test", "ws", "80", false, true},
		{"secure_websocket", "example.test", "wss", "443", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://"+tc.authority+"/", nil)
			r.RemoteAddr = "192.0.2.5:1234"
			r.Header.Set("Connection", "X-Auth, Upgrade")
			r.Header.Set("X-Auth", "untrusted")
			if tc.upgrade {
				r.Header.Set("Upgrade", "websocket")
			}
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			h := ingressHandler(&resolved.IngressPolicy{ForwardedHeaders: true}, http.HandlerFunc(func(_ http.ResponseWriter, got *http.Request) {
				if got.Header.Get("X-Forwarded-Proto") != tc.wantProto || got.Header.Get("X-Forwarded-Port") != tc.wantPort || got.Header.Get("X-Auth") != "" {
					t.Fatalf("ingress headers=%v", got.Header)
				}
				if (got.Header.Get("Connection") == "Upgrade") != tc.upgrade {
					t.Fatal("Connection upgrade preservation changed")
				}
			}))
			h.ServeHTTP(httptest.NewRecorder(), r)
		})
	}
}

func TestIngressLegacyAliasScope(t *testing.T) {
	for _, tc := range []struct {
		policy HeaderAliasPolicy
		status int
		dot    bool
	}{
		{HeaderAliasesDelete, 204, false},
		{HeaderUnderscoresDelete, 204, true},
		{HeaderUnderscoresReject, 400, false},
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("X_User", "value")
		r.Header.Set("X.User", "value")
		h := ingressHandler(&resolved.IngressPolicy{HeaderAliases: tc.policy}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X_User") != "" || (r.Header.Get("X.User") != "") != tc.dot {
				t.Fatalf("alias policy=%s headers=%v", tc.policy, r.Header)
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		if rec := runRequest(t, h, r); rec.Code != tc.status {
			t.Fatalf("policy=%s status=%d", tc.policy, rec.Code)
		}
	}
}

func TestIngressRejectsUnsupportedRequestTargets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*http.Request)
		status int
	}{
		{"opaque", func(r *http.Request) { r.URL.Opaque = "opaque" }, 400},
		{"connect", func(r *http.Request) { r.Method = http.MethodConnect }, 501},
		{"fragment", func(r *http.Request) { r.URL.RawPath = "/#" }, 400},
		{"truncated_escape", func(r *http.Request) { r.URL.RawPath = "/%" }, 400},
		{"invalid_escape", func(r *http.Request) { r.URL.RawPath = "/%zz" }, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			tc.mutate(r)
			h := ingressHandler(&resolved.IngressPolicy{NormalizePath: true}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("rejected target reached routing") }))
			if rec := runRequest(t, h, r); rec.Code != tc.status {
				t.Fatalf("status=%d want=%d", rec.Code, tc.status)
			}
		})
	}
}

func TestConditionalAndOrdinaryResponseHeadersShareOneRoute(t *testing.T) {
	h := chain(t, http.NotFoundHandler(), AddResponseHeader("X-Policy", "ordinary"), AddResponseHeader("X-Policy", "secure").OnHTTPS())
	for _, secure := range []bool{false, true, false} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if secure {
			r.TLS = &tls.ConnectionState{}
		}
		rec := runRequest(t, h, r)
		want := "ordinary"
		if secure {
			want += ",secure"
		}
		if strings.Join(rec.Header().Values("X-Policy"), ",") != want {
			t.Fatalf("TLS=%t headers=%v", secure, rec.Header())
		}
	}
}

func TestGraphIncludesIngressAndMethodPolicies(t *testing.T) {
	cfg := Config{
		Listeners: Listeners{
			HTTP(":8080").RedirectTo("https").RedirectStatus(302).RedirectNonGET(307).With(IngressPolicy{ForwardedHeaders: true, HeaderAliases: HeaderAliasesReject}),
			HTTPS(":8443", StaticTLS("/not-read/cert.pem", "/not-read/key.pem")),
		},
		Routes:         Routes{Match("/old").RedirectTo("https://new.example/", 301).RedirectNonGET(308).With(SetResponseHeader("X-Policy", "secure").OnHTTPS())},
		FallbackRoutes: Routes{Match("/*").RedirectTo("https://fallback.example/", 302)},
	}
	var out bytes.Buffer
	if err := GraphDOT(cfg, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"forwarded-headers=true", "aliases=reject", "redirect 302 (non-GET 307)", "non-GET 308", "HTTPS header: X-Policy", "terminal #1"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("graph omitted %q", want)
		}
	}
	if _, err := Resolve(Config{Listeners: Listeners{HTTP(":0")}, Routes: Routes{Match("/*").RedirectTo("/", 302).RedirectNonGET(200)}}); err == nil {
		t.Fatal("invalid non-GET redirect status accepted")
	}
}
