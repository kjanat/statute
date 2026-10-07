package statute

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"statute.kjanat.dev/resolved"
)

func TestRequestIDForbiddenOutputCategories(t *testing.T) {
	t.Parallel()
	for category, names := range map[string][]string{
		"framing":        {"Content-Length", "Transfer-Encoding", "Trailer", "Connection", "Upgrade"},
		"representation": {"Content-Encoding", "Content-Type", "Content-Range"},
		"validators":     {"ETag", "Last-Modified", "If-Match"},
		"caching":        {"Cache-Control", "Vary", "Expires", "Age"},
		"state/security": {"Set-Cookie", "Location", "Access-Control-Allow-Origin", "Access-Control-Allow-Credentials", "Strict-Transport-Security", "Content-Security-Policy", "WWW-Authenticate", "Alt-Svc"},
	} {
		t.Run(category, func(t *testing.T) {
			for _, name := range names {
				for _, spelling := range []string{name, strings.ToLower(name), strings.ToUpper(name)} {
					for _, from := range []string{"", "X-Foo"} {
						_, err := resolveMiddlewares([]Middleware{RequestID().Header(spelling).From(from)})
						if err == nil || !strings.Contains(err.Error(), "reserved") {
							t.Fatalf("%s from=%q: %v", spelling, from, err)
						}
					}
				}
			}
		})
	}
}

func TestRequestIDHeaderPolicyConfigurationBoundaries(t *testing.T) {
	t.Parallel()
	bad := RequestID().Header("Set-Cookie").From("X-Foo")
	for _, fallback := range []bool{false, true} {
		cfg := Config{Listeners: Listeners{HTTP(":0")}, Routes: Routes{Match("/*").Handle(http.NotFoundHandler()).With(bad)}}
		if fallback {
			cfg.FallbackRoutes, cfg.Routes = cfg.Routes, nil
		}
		if _, err := Resolve(cfg); err == nil {
			t.Fatalf("fallback=%v: unsafe output accepted", fallback)
		}
	}
	for _, cfg := range []*DockerConfig{Docker().DefaultMiddleware(bad), Docker().Middleware("bad", bad)} {
		if _, err := resolveDocker(cfg); err == nil {
			t.Fatal("Docker accepted unsafe output")
		}
	}
}

func TestRequestIDOutputHeaderControls(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", "X-Request-ID", "X-Trace", "Traceparent", "authorization", "cOoKiE"} {
		if _, err := resolveMiddlewares([]Middleware{RequestID().Header(name).From("Content-Type")}); err != nil {
			t.Fatalf("valid output %q rejected: %v", name, err)
		}
	}
	for _, name := range []string{" Content-Length", "Content-Length ", "X\r\nInjected", "Trailer:X-Request-Id", "X Identity"} {
		for _, mw := range []Middleware{RequestID().Header(name), RequestID().From(name)} {
			if _, err := resolveMiddlewares([]Middleware{mw}); err == nil {
				t.Fatalf("invalid name %q accepted", name)
			}
		}
	}
}

func TestRequestIDForbiddenSetDocumented(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("docs/request-id.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, names := range requestIDForbiddenOutputHeaders {
		for _, name := range names {
			if !strings.Contains(string(b), "`"+name+"`") {
				t.Errorf("missing documented prohibition: %s", name)
			}
			if _, err := resolveMiddlewares([]Middleware{RequestID().Header(name)}); err == nil {
				t.Errorf("named set member accepted: %s", name)
			}
		}
	}
}

// Invalid resolved input exercises the assembly defense after surface rejection.
func TestDockerRequestIDHeaderRefusal(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer backend.Close()
	host, portText, err := net.SplitHostPort(backend.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := resolveDocker(Docker().TraefikLabels().Middleware("bad", RequestID()).Middleware("good", RequestID()))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Middleware["bad"] = []resolved.Middleware{{Type: resolved.MWRequestID, RequestIDHeader: "sEt-CoOkIe", RequestIDFromHeader: "X-Foo"}}
	p, srv, _ := newFakeProvider(t, cfg, []fakeDaemonContainer{{
		name: "app", ip: host, port: port,
		labels: map[string]string{
			"traefik.enable":                        "true",
			"traefik.http.routers.bad.rule":         "Host(`bad.example.com`)",
			"traefik.http.routers.bad.service":      "app",
			"traefik.http.routers.bad.middlewares":  "bad",
			"traefik.http.routers.good.rule":        "Host(`good.example.com`)",
			"traefik.http.routers.good.service":     "app",
			"traefik.http.routers.good.middlewares": "good",
		},
	}})
	mustSync(t, p)
	for _, tc := range []struct {
		host   string
		status int
	}{{"bad.example.com", http.StatusNotFound}, {"good.example.com", http.StatusNoContent}} {
		r := httptest.NewRequest("GET", "http://"+tc.host+"/", nil)
		r.Header.Set("X-Foo", "session=attacker")
		h := findDynamicHandler(srv.dynamic.Load(), r.Host, r)
		if h == nil {
			t.Fatalf("%s fell through to fallback", tc.host)
		}
		rec := runRequest(t, h, r)
		if rec.Code != tc.status || rec.Header().Get("Set-Cookie") != "" {
			t.Fatalf("%s: status=%d headers=%v", tc.host, rec.Code, rec.Header())
		}
	}
}
