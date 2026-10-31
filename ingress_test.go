package statute

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"statute.kjanat.dev/resolved"
)

func TestIngressRoutingIdentityRateLimitAndObservability(t *testing.T) {
	var logs bytes.Buffer
	calls := 0
	cfg, err := Resolve(Config{
		Listeners: Listeners{HTTP(":8080").With(IngressPolicy{ForwardedHeaders: true, NormalizePath: true, SanitizePath: true, HeaderAliases: HeaderAliasesReject})},
		Routes: Routes{
			Match("/admin").Handle(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(200) })).With(AllowIPs("127.0.0.1/32"), RateLimit("1/h")),
			Match("/private").Handle(http.NotFoundHandler()).With(AllowIPs("198.51.100.0/24")),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Observability.AccessLog = resolved.AccessLog{Enabled: true, Writer: &logs, SampleRate: 1}
	s := &server{cfg: cfg, stats: newStats()}
	h := s.buildListenerHandler(cfg.Listeners[0], s.buildRouter(), nil, nil)
	for _, test := range []struct {
		path, forwarded, alias string
		status                 int
	}{
		{"/a/../admin", "198.51.100.1", "", 200},
		{"/admin", "203.0.113.1", "", 429},
		{"/private", "198.51.100.1", "", 403},
		{"/admin", "203.0.113.1", "X.Auth.User", 400},
	} {
		r := httptest.NewRequest(http.MethodGet, "http://example.test"+test.path, nil)
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set(headerXForwardedFor, test.forwarded)
		if test.alias != "" {
			r.Header.Set(test.alias, "admin")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != test.status {
			t.Fatalf("path=%s status=%d want=%d", test.path, rec.Code, test.status)
		}
	}
	if calls != 1 || s.stats.requests.Load() != 4 {
		t.Fatalf("calls=%d metrics=%d", calls, s.stats.requests.Load())
	}
	assertIngressAccessLogs(t, logs.Bytes())
}

func assertIngressAccessLogs(t *testing.T, data []byte) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	for i := range 4 {
		var row map[string]any
		if err := decoder.Decode(&row); err != nil {
			t.Fatal(err)
		}
		if row["remote"] != "127.0.0.1" {
			t.Fatalf("spoofed log identity: %+v", row)
		}
		if i < 3 && row["forwarded_for"] != "" {
			t.Fatalf("unsanitized log headers: %+v", row)
		}
		if i == 0 && row["path"] != "/a/../admin" {
			t.Fatalf("original log path lost: %+v", row)
		}
	}
}

func TestIngressForwardedTrustAndAliases(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "trusted"}[trusted], func(t *testing.T) {
			var got *http.Request
			next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r })
			p := &resolved.IngressPolicy{ForwardedHeaders: true, HeaderAliases: HeaderAliasesDelete}
			h := ingressHandler(p, next)
			if trusted {
				h = trustedProxyMiddleware(&resolved.Listener{TrustedProxies: []string{"192.0.2.0/24"}, ClientIPHeader: "X-Forwarded-For"}, h)
			}
			r := httptest.NewRequest(http.MethodGet, "http://app.example.test/a", nil)
			r.RemoteAddr = "192.0.2.5:1234"
			r.Header.Set("X-Forwarded-For", "198.51.100.7")
			r.Header.Set("X-Forwarded-Proto", "https")
			r.Header.Set("X-Real-IP", "198.51.100.7")
			r.Header.Set("X_Forwarded_For", "203.0.113.99")
			r.Header.Set("X.Auth.User", "admin")
			r.Header.Set("Connection", "X-Forwarded-For, X-Auth-Input")
			r.Header.Set("X-Auth-Input", "forged")
			h.ServeHTTP(httptest.NewRecorder(), r)
			want := "192.0.2.5"
			if trusted {
				want = "198.51.100.7"
			}
			assertIngressIdentity(t, got, want)
			if trusted && got.Header.Get("X-Forwarded-Port") != "443" {
				t.Fatal("forwarded scheme lost its port")
			}
			out := http.Header{}
			applyIngressForwarded(got, out)
			if trusted && out.Get("X-Forwarded-For") != "198.51.100.7, 192.0.2.5" {
				t.Fatalf("forwarded chain=%q", out.Get("X-Forwarded-For"))
			}
		})
	}
}

func TestIngressPathBeforeRoutingAndRewrite(t *testing.T) {
	for _, input := range []string{"/a/../admin?x=1", "/%61//%2e%2e/admin?x=1"} {
		r := httptest.NewRequest(http.MethodGet, "http://example.test"+input, nil)
		uri := ""
		h := ingressHandler(&resolved.IngressPolicy{NormalizePath: true, SanitizePath: true, HeaderAliases: HeaderAliasesKeep}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/admin" || r.RequestURI != "/admin?x=1" {
				t.Fatalf("normalized path=%s uri=%s", r.URL.Path, r.RequestURI)
			}
			uri = r.URL.RequestURI()
		}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if uri != "/admin?x=1" || r.URL.RequestURI() != input {
			t.Fatalf("output=%q original=%s", uri, r.URL.RequestURI())
		}
	}
}

func TestIngressPolicyResolution(t *testing.T) {
	for _, l := range []*Listener{
		HTTP(":8080").With(IngressPolicy{HeaderAliases: "invalid"}),
		HTTP(":8080").With(IngressPolicy{}, IngressPolicy{}),
		HTTP(":8080").With(StaticTLS("certificate", "key")),
	} {
		if _, err := resolveListener(l); err == nil {
			t.Fatal("invalid listener policy accepted")
		}
	}
	l, err := resolveListener(HTTP(":8080").With(IngressPolicy{NormalizePath: true}, TrustedProxy("192.0.2.0/24")))
	if err != nil || l.Ingress == nil || !l.Ingress.NormalizePath || len(l.TrustedProxies) != 1 {
		t.Fatalf("listener=%+v err=%v", l, err)
	}
}

func TestIngressRejectAndDefaultBehavior(t *testing.T) {
	for _, p := range []*resolved.IngressPolicy{nil, {HeaderAliases: HeaderAliasesReject}} {
		r := httptest.NewRequest(http.MethodGet, "http://example.test/a/../b", nil)
		r.Header.Set("X_Auth", "forged")
		r.TLS = &tls.ConnectionState{}
		rec := httptest.NewRecorder()
		path := ""
		ingressHandler(p, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { path = r.URL.Path })).ServeHTTP(rec, r)
		if p == nil && !strings.Contains(path, "/a/../b") {
			t.Fatal("default listener behavior changed")
		}
		if p != nil && rec.Code != http.StatusBadRequest {
			t.Fatal("alias rejection served content")
		}
	}
}

func assertIngressIdentity(t *testing.T, r *http.Request, want string) {
	t.Helper()
	if r == nil {
		t.Fatal("ingress did not forward the request")
	}
	if clientIP(r) != want || r.Header.Get(headerXRealIP) != want {
		t.Fatalf("identity=%s real-ip=%s want=%s", clientIP(r), r.Header.Get(headerXRealIP), want)
	}
	for _, name := range []string{"X_Forwarded_For", "X.Auth.User", "X-Auth-Input"} {
		if r.Header.Get(name) != "" {
			t.Fatalf("spoofed header %s survived", name)
		}
	}
}
