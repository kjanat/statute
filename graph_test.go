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

func TestGraphDOTShowsClientAuth(t *testing.T) {
	t.Parallel()
	cfg := tlsRouterConfig(
		StaticTLS("cert.pem", "key.pem"),
		ClientAuth{Mode: RequireAndVerifyClientCert, CAFiles: []string{"/run/certs/client-ca.pem"}},
	)
	var buf bytes.Buffer
	if err := GraphDOT(cfg, &buf); err != nil {
		t.Fatalf("GraphDOT: %v", err)
	}
	for _, want := range []string{"client-auth=require-and-verify", "client-ca=[/run/certs/client-ca.pem]"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("graph missing %q:\n%s", want, buf.String())
		}
	}
}

func TestGraphDOT_ShowsDockerPoolPolicy(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Listeners: Listeners{HTTP(":80")},
		Docker: Docker().
			PoolPolicy("app@traefik", PoolPolicy{
				HealthCheck: HealthCheck{
					Path: "/ready", Interval: "13s", Timeout: "4s", Healthy: 4, Unhealthy: 5,
					Host: "probe.internal", Statuses: []int{200, 204},
				},
				PassiveHealthCheck: PassiveHealthCheck{FailureWindow: "30s", MaxFailures: 3},
				Transport: Transport{
					MaxIdleConnsPerHost: 17, IdleConnTimeout: "71s", DialTimeout: "6s", TLSHandshakeTimeout: "7s",
					ResponseHeaderTimeout: "8s", FlushInterval: "9s", ServerName: "app.internal",
					RootCAFiles: []string{"/run/certs/root.pem"},
					ClientCertificate: ClientCertificate{
						CertFile: "/run/certs/client.crt", KeyFile: "/run/certs/client.key",
					},
				},
				UpstreamHost: HostValue("app.internal"),
			}).
			PoolPolicy("app_traefik", PoolPolicy{Transport: Transport{InsecureSkipVerify: true}}),
	}
	var buf bytes.Buffer
	if err := GraphDOT(cfg, &buf); err != nil {
		t.Fatalf("GraphDOT: %v", err)
	}
	graph := buf.String()
	for _, want := range []string{
		"DP_0", "DP_1", "app@traefik", "app_traefik", "Docker pool policy", "host=app.internal",
		"health=active(path=/ready,interval=13s,timeout=4s,healthy=4,unhealthy=5,host=probe.internal,statuses=[200 204])+passive(window=30s,max=3)",
		"transport=custom-tls,sni=app.internal,roots=[/run/certs/root.pem],max-idle=17,idle=1m11s,dial=6s,handshake=7s,header=8s,flush=9s",
		"client-cert=/run/certs/client.crt,client-key=/run/certs/client.key",
		"transport=insecure-tls,sni=,roots=[],max-idle=32,idle=1m30s,dial=5s,handshake=5s,header=0s,flush=0s",
	} {
		if !strings.Contains(graph, want) {
			t.Errorf("graph missing %q:\n%s", want, graph)
		}
	}
	if strings.Count(graph, "DP_0 [") != 1 || strings.Count(graph, "DP_1 [") != 1 {
		t.Errorf("policy node identifiers are not distinct:\n%s", graph)
	}
}

func TestGraphDOT_ShowsDockerWorkload(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Listeners: Listeners{HTTP(":8080")},
		Docker: Docker().Workload("app@traefik", WorkloadPolicy{
			IdleAfter: "10m",
			Readiness: HTTPReadiness("/healthz"),
		}),
	}
	var buf bytes.Buffer
	if err := GraphDOT(cfg, &buf); err != nil {
		t.Fatalf("GraphDOT: %v", err)
	}
	dot := buf.String()
	for _, want := range []string{"DW_0", "Docker workload", "app@traefik", "idle_after=10m", "readiness=http:/healthz"} {
		if !strings.Contains(dot, want) {
			t.Errorf("DOT output misses %q:\n%s", want, dot)
		}
	}
}
