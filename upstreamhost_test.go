package statute

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"statute.kjanat.dev/resolved"
)

// hostPoolConfig routes everything to one backend under the given Host
// policy.
func hostPoolConfig(backendURL string, policy UpstreamHost) Config {
	return Config{
		Listeners: Listeners{HTTP(":0")},
		Upstreams: Upstreams{
			"api": Pool{
				Backends:     []Backend{{Address: strings.TrimPrefix(backendURL, "http://")}},
				UpstreamHost: policy,
			},
		},
		Routes: Routes{Match("/*").ProxyTo("api")},
	}
}

// TestProxyUpstreamHostPolicies — each policy controls the Host header the
// backend sees: the default forwards the client's, TargetHost sends the
// backend its own, and HostValue sends the fixed name.
func TestProxyUpstreamHostPolicies(t *testing.T) {
	t.Parallel()
	backend := newEchoBackend(t)
	targetHost := strings.TrimPrefix(backend.URL, "http://")

	cases := []struct {
		name   string
		policy UpstreamHost
		want   string
	}{
		{"default preserves the client host", ClientHost, "client.example.com"},
		{"zero value is the default", UpstreamHost{}, "client.example.com"},
		{"target host", TargetHost, targetHost},
		{"explicit value", HostValue("api.internal.example"), "api.internal.example"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv, err := newServer(mustResolve(t, hostPoolConfig(backend.URL, c.policy)))
			if err != nil {
				t.Fatalf("newServer: %v", err)
			}
			t.Cleanup(func() {
				for _, ph := range srv.pools {
					ph.shutdown()
				}
			})
			req := httptest.NewRequest("GET", "http://client.example.com/x", nil)
			rec := runRequest(t, srv.buildRouter(), req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status: got %d, want 200", rec.Code)
			}
			if got := decodeEcho(t, rec.Body).Host; got != c.want {
				t.Errorf("upstream Host: got %q, want %q", got, c.want)
			}
		})
	}
}

// TestProbeHostPolicy — a probe carries the pool's explicit Host, and under
// the other policies stays on the backend's own host; there is no client
// Host to preserve.
func TestProbeHostPolicy(t *testing.T) {
	t.Parallel()
	hosts := make(chan string, 4)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hosts <- r.Host
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)
	targetHost := strings.TrimPrefix(backend.URL, "http://")
	cfg := resolved.HealthCheck{
		Enabled: true, Path: "/healthz",
		Interval: time.Hour, Timeout: time.Second,
		Healthy: 1, Unhealthy: 1,
	}

	probeOnce := func(host string) string {
		t.Helper()
		b := &backendState{backend: &resolved.Backend{Address: targetHost}}
		b.markHealthy(true)
		hc := newHealthChecker(cfg, []*backendState{b}, nil, host)
		hc.probe(context.Background(), b)
		if !b.isHealthy() {
			t.Fatal("probe failed against a live backend")
		}
		select {
		case h := <-hosts:
			return h
		case <-time.After(2 * time.Second):
			t.Fatal("no probe observed")
			return ""
		}
	}

	if got := probeOnce(""); got != targetHost {
		t.Errorf("default probe Host: got %q, want %q", got, targetHost)
	}
	if got := probeOnce("api.internal.example"); got != "api.internal.example" {
		t.Errorf("explicit probe Host: got %q, want %q", got, "api.internal.example")
	}
}

// TestResolveUpstreamHost — the three policies land in the resolved schema,
// and an explicit value is validated like the header it becomes.
func TestResolveUpstreamHost(t *testing.T) {
	t.Parallel()
	pools := func(u UpstreamHost) *resolved.Pool {
		t.Helper()
		r := mustResolve(t, hostPoolConfig("http://127.0.0.1:9001", u))
		return r.Upstreams["api"]
	}
	if p := pools(UpstreamHost{}); p.UpstreamHost != resolved.HostClient || p.HostValue != "" {
		t.Errorf("zero policy: %+v", p)
	}
	if p := pools(TargetHost); p.UpstreamHost != resolved.HostTarget {
		t.Errorf("target policy: %+v", p)
	}
	if p := pools(HostValue("api.internal:8443")); p.UpstreamHost != resolved.HostExplicit || p.HostValue != "api.internal:8443" {
		t.Errorf("explicit policy: %+v", p)
	}

	for name, bad := range map[string]string{
		"empty":     "",
		"blank":     "   ",
		"injection": "evil\r\nX-Injected: yes",
	} {
		_, err := Resolve(hostPoolConfig("http://127.0.0.1:9001", HostValue(bad)))
		if err == nil || !strings.Contains(err.Error(), "upstream_host") {
			t.Errorf("%s HostValue: got %v, want upstream_host error", name, err)
		}
	}
}

// TestUpstreamHostString — the labels the policies print under.
func TestUpstreamHostString(t *testing.T) {
	t.Parallel()
	cases := map[string]UpstreamHost{
		"client_host":       ClientHost,
		"target_host":       TargetHost,
		"host:api.internal": HostValue("api.internal"),
		enumUnknown:         {mode: hostMode(99)},
	}
	for want, u := range cases {
		if got := u.String(); got != want {
			t.Errorf("String: got %q, want %q", got, want)
		}
	}
}
