package statute

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"statute.kjanat.dev/resolved"
)

func TestDockerMiddlewareRejectsUnknownListener(t *testing.T) {
	for _, defaults := range []bool{false, true} {
		for _, name := range []string{"web", "weeb"} {
			chain := OnListeners([]string{name}, InOrder(BasicAuth("private", map[string]string{"test": hunter2Hash})))
			docker := Docker().TraefikLabels()
			if defaults {
				docker.DefaultMiddleware(chain)
			} else {
				docker.Middleware("auth", chain)
			}
			_, err := Resolve(Config{Listeners: Listeners{HTTP(":8080").Name("web")}, Docker: docker})
			if name == "web" && err != nil {
				t.Fatalf("valid listener rejected: %v", err)
			}
			if name == "weeb" && (err == nil || !strings.Contains(err.Error(), "unknown listener")) {
				t.Fatalf("defaults=%t accepted unavailable auth listener: %v", defaults, err)
			}
		}
	}
}

func TestRoutePriorityKeepsPeerPathAndListenerConstraints(t *testing.T) {
	cfg := Config{PrioritizeRoutes: true, Listeners: Listeners{HTTP(":8080").Name("public"), HTTP(":8081").Name("private")}, Routes: Routes{
		MatchPrefix("/api").Hosts("app.example").PeerIPs("192.0.2.0/24").OnListeners("private").Priority(30).Handle(statusHandler(201)).With(InOrder(SetResponseHeader("X-Route", "private"))).WithOuter(SetResponseHeader("X-Outer", "yes")),
		MatchExact("/api").Host("app.example").Priority(20).Handle(statusHandler(202)),
		Match("/*").Priority(1).Handle(statusHandler(203)),
	}}
	srv, err := newServer(mustResolve(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown() })
	for _, tc := range []struct {
		name, path, peer string
		status           int
	}{
		{"private", "/apiary", "192.0.2.1:42", 201},
		{"public", "/apiary", "192.0.2.1:42", 203},
		{"private", "/apiary", "198.51.100.1:42", 203},
		{"public", "/api", "192.0.2.1:42", 202},
		{"private", "/api", "invalid", 202},
	} {
		r := httptest.NewRequest(http.MethodGet, "http://app.example"+tc.path, nil)
		r.RemoteAddr = tc.peer
		w := httptest.NewRecorder()
		namedListener(tc.name, srv.buildRouter()).ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("listener=%s path=%s peer=%s status=%d", tc.name, tc.path, tc.peer, w.Code)
		}
	}
}

func statusHandler(status int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) })
}

func TestRouteRejectsUnknownListenerAndEmptyPeerConstraint(t *testing.T) {
	for _, cfg := range []Config{
		{Listeners: Listeners{HTTP(":8080").Name("same"), HTTP(":8081").Name("same")}},
		{Listeners: Listeners{HTTP(":8080").Name("public")}, Routes: Routes{Match("/*").OnListeners("missing").Handle(http.NotFoundHandler())}},
		{Listeners: Listeners{HTTP(":8080").Name("public")}, Routes: Routes{Match("/*").Handle(http.NotFoundHandler()).With(OnListeners([]string{"missing"}, InOrder(SetResponseHeader("X-Test", "value"))))}},
		{Listeners: Listeners{HTTP(":8080")}, Routes: Routes{Match("/*").PeerIPs().Handle(http.NotFoundHandler())}},
		{Listeners: Listeners{HTTP(":8080")}, Routes: Routes{Match("/*").PeerIPs("invalid").Handle(http.NotFoundHandler())}},
	} {
		if _, err := Resolve(cfg); err == nil {
			t.Fatal("invalid routing constraint accepted")
		}
	}
}

func TestPrioritizedDockerRefusalCannotReachBroaderRoute(t *testing.T) {
	cfg, err := resolveDocker(Docker().TraefikLabels().RouterConstraints().DefaultListeners("web"))
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{"traefik.enable": "true", "traefik.http.routers.private.rule": "Host(`app.example`) && PathPrefix(`/admin`)", "traefik.http.routers.private.middlewares": "missing-auth", "traefik.http.routers.private.priority": "100"}
	p, srv, _ := newFakeProvider(t, cfg, []fakeDaemonContainer{{name: "app", ip: "127.0.0.1", port: 8080, labels: labels}})
	srv.cfg.PrioritizeRoutes = true
	srv.cfg.Routes = []*resolved.Route{{Pattern: "/*", Priority: 10, HandlerRoute: true, Handler: statusHandler(http.StatusTeapot)}}
	mustSync(t, p)
	w := httptest.NewRecorder()
	namedListener("web", srv.buildRouter()).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://app.example/admin", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("refused route reached broad handler: %d", w.Code)
	}
}

func TestPrioritizedUnsupportedDockerRuleCannotBypassMissingAuth(t *testing.T) {
	cfg, err := resolveDocker(Docker().TraefikLabels())
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{"traefik.enable": "true", "traefik.http.routers.private.rule": "Host(`app.example`) && Header(`X-Private`, `yes`)", "traefik.http.routers.private.middlewares": "missing-auth", "traefik.http.routers.private.priority": "100"}
	labels["traefik.http.routers.global.rule"] = "Header(`X-Global`, `yes`)"
	labels["traefik.http.routers.global.priority"] = "1"
	p, srv, _ := newFakeProvider(t, cfg, []fakeDaemonContainer{{name: "app", ip: "127.0.0.1", port: 8080, labels: labels}})
	srv.cfg.PrioritizeRoutes = true
	srv.cfg.Routes = []*resolved.Route{{Pattern: "/*", Priority: 10, HandlerRoute: true, Handler: statusHandler(http.StatusTeapot)}}
	mustSync(t, p)
	w := runRequest(t, srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://app.example/admin", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("unsupported protected rule reached broader handler: %d", w.Code)
	}
}
