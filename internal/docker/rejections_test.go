package docker

import "testing"

func TestExtractParsedRejections(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
		want   int
	}{
		{"invalid native backend", map[string]string{"statute.enable": "true", "statute.host": "app.example.com", "statute.path": "/admin/*", "statute.port": "bad"}, 1},
		{"invalid native enable", map[string]string{"statute.enable": "bad", "statute.host": "app.example.com", "statute.path": "/admin/*"}, 1},
		{"invalid Traefik backend", map[string]string{"traefik.enable": "true", "traefik.http.routers.admin.rule": "Host(`app.example.com`) && PathPrefix(`/admin`)", "traefik.http.services.app.loadbalancer.server.port": "bad"}, 1},
		{"invalid Traefik enable", map[string]string{"traefik.enable": "bad", "traefik.http.routers.admin.rule": "Host(`app.example.com`) && PathPrefix(`/admin`)"}, 1},
		{"unusable network", map[string]string{"traefik.enable": "true", "traefik.docker.network": "missing", "traefik.http.routers.admin.rule": "Host(`app.example.com`) && PathPrefix(`/admin`)"}, 1},
		{"healthy router", map[string]string{"traefik.enable": "true", "traefik.http.routers.admin.rule": "Host(`app.example.com`) && PathPrefix(`/admin`)"}, 0},
		{"unsupported rule", map[string]string{"traefik.enable": "true", "traefik.http.routers.admin.rule": "Host(`app.example.com`) && ClientIP(`10.0.0.0/8`)"}, 0},
		{"malformed rule", map[string]string{"traefik.enable": "true", "traefik.http.routers.admin.rule": "Host("}, 0},
		{"opt out", map[string]string{"traefik.enable": "false", "traefik.http.routers.admin.rule": "PathPrefix(`/admin`)"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, rejected, _ := ExtractWithRejections(webContainer(tc.labels), ExtractOptions{TraefikLabels: true})
			if len(rejected) != tc.want {
				t.Fatalf("rejected=%+v, want %d", rejected, tc.want)
			}
			for _, m := range rejected {
				if !m.Match("app.example.com", "/admin/users") || m.Match("app.example.com", "/public") || m.Match("other.example.com", "/admin/users") {
					t.Fatalf("lost original rejection predicate: %+v", m)
				}
			}
		})
	}
}
