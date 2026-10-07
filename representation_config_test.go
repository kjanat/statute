package statute

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"statute.kjanat.dev/internal/docker"
	"statute.kjanat.dev/resolved"
)

func TestResolveCompressionHeaderConflict(t *testing.T) {
	for _, op := range []struct {
		name string
		mw   Middleware
		want string
	}{
		{"remove", RemoveResponseHeader("cOnTeNt-EnCoDiNg"), "cannot modify Content-Encoding with Compress"},
		{"set", SetResponseHeader("Content-Encoding", "identity"), "cannot modify Content-Encoding with Compress"},
		{"add", AddResponseHeader("Content-Encoding", "gzip"), "cannot modify Content-Encoding with Compress"},
		{"request ID", RequestID().Header("cOnTeNt-EnCoDiNg"), "reserved"},
		{"forwarded request ID", RequestID().Header("Content-Encoding").From("X-Request-Id"), "reserved"},
	} {
		t.Run(op.name, func(t *testing.T) {
			for _, codec := range []CompressAlgo{Gzip, Brotli} {
				for _, reverse := range []bool{false, true} {
					mws := []Middleware{op.mw, Compress(codec), Cache("1m"), Retry(2, OnStatus(503)), ETag()}
					if reverse {
						mws[0], mws[1] = mws[1], mws[0]
					}
					for _, fallback := range []bool{false, true} {
						cfg := Config{Listeners: Listeners{HTTP(":8080")}, Routes: Routes{Match("/*").Handle(http.NotFoundHandler()).With(mws...)}}
						path := "route[0]"
						if fallback {
							cfg.FallbackRoutes, cfg.Routes = cfg.Routes, nil
							path = "fallback_routes[0]"
						}
						_, err := Resolve(cfg)
						if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), op.want) {
							t.Fatalf("codec=%v reverse=%t fallback=%t: %v", codec, reverse, fallback, err)
						}
					}
				}
			}
		})
	}
}

func TestResolveCompressionHeaderControls(t *testing.T) {
	for _, mws := range [][]Middleware{
		{RemoveResponseHeader("Content-Encoding")},
		{SetResponseHeader("Content-Encoding", "gzip")},
		{Compress(Gzip), RemoveResponseHeader("ETag"), RemoveResponseHeader("Content-Length"), SetResponseHeader("X-Test", "ok")},
		{Compress(Gzip), RemoveRequestHeader("Content-Encoding")},
		{Compress(Gzip), RequestID()},
		{Compress(), RemoveResponseHeader("Content-Encoding")},
	} {
		if _, err := resolveMiddlewares(mws); err != nil {
			t.Fatal(err)
		}
	}
	for _, cfg := range []*DockerConfig{
		Docker().DefaultMiddleware(Compress(Gzip), RemoveResponseHeader("Content-Encoding")),
		Docker().Middleware("bad", RemoveResponseHeader("Content-Encoding"), Compress(Gzip)),
	} {
		if _, err := resolveDocker(cfg); err == nil || !strings.Contains(err.Error(), "cannot modify Content-Encoding") {
			t.Fatalf("Docker configuration: %v", err)
		}
	}
}

func TestDockerAssembledCompressionHeaderConflict(t *testing.T) {
	remove := resolved.Middleware{Type: resolved.MWRemoveResponseHeader, HeaderName: "cOnTeNt-EnCoDiNg"}
	compress := resolved.Middleware{Type: resolved.MWCompress, CompressAlgos: []resolved.CompressAlgo{resolved.Gzip}}
	for _, tc := range []struct {
		name                   string
		defaults, named, hints []resolved.Middleware
	}{
		{"defaults", []resolved.Middleware{remove}, []resolved.Middleware{compress}, nil},
		{"named", []resolved.Middleware{compress}, []resolved.Middleware{remove}, nil},
		{"hints", nil, []resolved.Middleware{remove}, []resolved.Middleware{compress}},
		{"request ID", nil, []resolved.Middleware{{Type: resolved.MWRequestID, RequestIDHeader: "cOnTeNt-EnCoDiNg", RequestIDFromHeader: "X-Request-Id"}}, []resolved.Middleware{compress}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &dockerProvider{cfg: &resolved.Docker{DefaultMiddleware: tc.defaults, Middleware: map[string][]resolved.Middleware{"policy": tc.named}}}
			mws, warning := p.routeMiddleware(&docker.Service{Name: "app"}, docker.Matcher{Host: "app.example.com", Path: "/", Middlewares: []string{"policy"}}, tc.hints)
			if mws != nil || !strings.Contains(warning, "cannot modify Content-Encoding") {
				t.Fatalf("mws=%v warning=%q", mws, warning)
			}
		})
	}
}

func TestDockerCompressionConflictRefusesOnlyAffectedRoute(t *testing.T) {
	cfg, err := resolveDocker(Docker().TraefikLabels().
		Middleware("remove", RemoveResponseHeader("Content-Encoding")).
		Middleware("compress", Compress(Gzip)))
	if err != nil {
		t.Fatal(err)
	}
	p, srv, _ := newFakeProvider(t, cfg, []fakeDaemonContainer{{
		name: "app", ip: "10.0.0.9", port: 3000,
		labels: map[string]string{
			"traefik.enable":                        "true",
			"traefik.http.routers.bad.rule":         "Host(`bad.example.com`)",
			"traefik.http.routers.bad.service":      "app",
			"traefik.http.routers.bad.middlewares":  "remove,compress",
			"traefik.http.routers.good.rule":        "Host(`good.example.com`)",
			"traefik.http.routers.good.service":     "app",
			"traefik.http.routers.good.middlewares": "compress",
		},
	}})
	mustSync(t, p)
	tab := srv.dynamic.Load()
	if len(tab.routes) != 1 || tab.routes[0].route.Host != "good.example.com" {
		t.Fatalf("routes=%+v", tab.routes)
	}
	h := findDynamicHandler(tab, "bad.example.com", httptest.NewRequest("GET", "http://bad.example.com/", nil))
	if h == nil {
		t.Fatal("invalid route fell through instead of refusing")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://bad.example.com/", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("refusal=%d", rec.Code)
	}
}
