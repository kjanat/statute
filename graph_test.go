package statute

import (
	"bytes"
	"strings"
	"testing"
)

func TestGraphDOT_ContainsExpectedNodes(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Listeners: Listeners{
			HTTP(":80").RedirectTo("https"),
			HTTPS(":443",
				StaticTLS("/etc/cert.pem", "/etc/key.pem"),
				HTTP2(),
			),
		},
		Upstreams: Upstreams{
			"api": Pool{
				Backends: []Backend{
					{Address: "10.0.0.1:8080"},
					{Address: "10.0.0.2:8080", Backup: true},
				},
				Strategy: LeastConnections,
			},
		},
		Routes: Routes{
			Match("/api/*").Host("api.example.com").ProxyTo("api"),
		},
	}
	var buf bytes.Buffer
	if err := GraphDOT(cfg, &buf); err != nil {
		t.Fatalf("GraphDOT: %v", err)
	}
	out := buf.String()
	wants := []string{
		"digraph statute {",
		"rankdir=LR",
		":80",
		":443",
		"least-conn",
		"10.0.0.1:8080",
		"10.0.0.2:8080",
		"api.example.com",
		"redirect 301",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("graph missing %q in output:\n%s", w, out)
		}
	}
}

func TestGraphDOT_BadConfigReturnsError(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	err := GraphDOT(Config{
		Listeners: Listeners{HTTP(":80")},
		Routes:    Routes{Match("/*").ProxyTo("missing")},
	}, &buf)
	if err == nil {
		t.Fatal("want error for bad config")
	}
}

func TestGraphDOT_ShowsDockerPoolPolicy(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Listeners: Listeners{HTTP(":80")},
		Docker: Docker().PoolPolicy("app@traefik", PoolPolicy{
			HealthCheck:        HealthCheck{Path: "/ready"},
			PassiveHealthCheck: PassiveHealthCheck{FailureWindow: "30s", MaxFailures: 3},
			Transport:          Transport{ServerName: "app.internal", RootCAFiles: []string{"/run/certs/root.pem"}},
			UpstreamHost:       HostValue("app.internal"),
		}),
	}
	var buf bytes.Buffer
	if err := GraphDOT(cfg, &buf); err != nil {
		t.Fatalf("GraphDOT: %v", err)
	}
	graph := buf.String()
	for _, want := range []string{"app@traefik", "Docker pool policy", "host=app.internal", "health=active+passive", "transport=custom-tls,sni=app.internal,roots=1"} {
		if !strings.Contains(graph, want) {
			t.Errorf("graph missing %q:\n%s", want, graph)
		}
	}
}
