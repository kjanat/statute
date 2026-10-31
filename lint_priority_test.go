package statute

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"statute.kjanat.dev/resolved"
)

func TestLint_FB001FollowsPrioritizedSelection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		priority   int
		prioritize bool
		terminal   bool
		want       []string
		unwant     []string
	}{
		{name: "declaration order", priority: 1, want: []string{"every Docker-discovered route, and the refusals"}},
		{name: "prioritized docker", priority: 1, prioritize: true, want: []string{"priority 1 or lower", "native statute.* label route"}, unwant: []string{"every Docker-discovered route,"}},
		{name: "prioritized below docker defaults", priority: -1, prioritize: true},
		{name: "prioritized terminal stages", priority: -1, prioritize: true, terminal: true, want: []string{"terminal fallback routes", "the fallback"}, unwant: []string{"Docker"}},
		{name: "prioritized docker and terminal", priority: 5, prioritize: true, terminal: true, want: []string{"priority 5 or lower", "terminal fallback routes", "the fallback"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := Config{
				PrioritizeRoutes: tc.prioritize,
				Listeners:        Listeners{HTTP(":8080").Name("web")},
				Upstreams:        Upstreams{"default": Pool{Backends: []Backend{{Address: "http://127.0.0.1:9000"}}}},
				Routes:           Routes{Match("/*").Priority(tc.priority).OnListeners("web").ProxyTo("default")},
				Docker:           Docker().TraefikLabels().RouterConstraints().DefaultListeners("web"),
			}
			if tc.terminal {
				cfg.FallbackRoutes = Routes{Match("/*").Handle(noContentHandler)}
				cfg.Fallback = http.NotFoundHandler()
			}
			assertFB001Message(t, cfg, tc.want, tc.unwant)
		})
	}
}

func fb001Findings(t *testing.T, cfg Config) []Finding {
	t.Helper()
	findings, err := Lint(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var got []Finding
	for _, finding := range findings {
		if finding.Code == "FB001" {
			got = append(got, finding)
		}
	}
	return got
}

func assertFB001Message(t *testing.T, cfg Config, want, unwant []string) {
	t.Helper()
	got := fb001Findings(t, cfg)
	if len(want) == 0 {
		if len(got) != 0 {
			t.Fatalf("FB001 = %v, want none", got)
		}
		return
	}
	if len(got) != 1 || got[0].Path != "routes[0]" {
		t.Fatalf("FB001 = %v, want one at routes[0]", got)
	}
	for _, text := range want {
		if !strings.Contains(got[0].Message, text) {
			t.Errorf("message %q lacks %q", got[0].Message, text)
		}
	}
	for _, text := range unwant {
		if strings.Contains(got[0].Message, text) {
			t.Errorf("message %q claims %q", got[0].Message, text)
		}
	}
}

func TestPrioritizedCatchAllShadowsOnlyLowerDockerPriorities(t *testing.T) {
	cfg, err := resolveDocker(Docker().TraefikLabels())
	if err != nil {
		t.Fatal(err)
	}
	p, srv, _ := newFakeProvider(t, cfg, []fakeDaemonContainer{
		{name: "traefik", ip: "127.0.0.1", port: 1, labels: map[string]string{"traefik.enable": "true", "traefik.http.routers.app.rule": "Host(`traefik.example`)"}},
		{name: "native", ip: "127.0.0.1", port: 1, labels: map[string]string{"statute.host": "native.example"}},
	})
	srv.cfg.PrioritizeRoutes = true
	srv.cfg.Routes = []*resolved.Route{{Pattern: "/*", Priority: 1, HandlerRoute: true, Handler: statusHandler(http.StatusTeapot)}}
	mustSync(t, p)
	for host, static := range map[string]bool{"traefik.example": false, "native.example": true} {
		w := runRequest(t, srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil))
		if (w.Code == http.StatusTeapot) != static {
			t.Errorf("%s status=%d static=%v", host, w.Code, static)
		}
	}
}
