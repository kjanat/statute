package statute

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/net/idna"
)

func healthRedirectTLS(t *testing.T, pki clientAuthPKI, handler http.Handler, connections *atomic.Int64) *httptest.Server {
	t.Helper()
	cert, err := tls.LoadX509KeyPair(pki.serverCertFile, pki.serverKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(handler)
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{cert}, ClientAuth: tls.VerifyClientCertIfGiven,
		ClientCAs: pki.serverRoots, MinVersion: tls.VersionTLS12,
	}
	if connections != nil {
		srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
			if state == http.StateNew {
				connections.Add(1)
			}
		}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func healthRedirectPool(t *testing.T, pki clientAuthPKI, source string, hc HealthCheck, identity bool) *poolHandler {
	t.Helper()
	tr := Transport{ServerName: "x.example", RootCAFiles: []string{pki.caFile}}
	if identity {
		tr.ClientCertificate = ClientCertificate{CertFile: pki.clientCertFile, KeyFile: pki.clientKeyFile}
	}
	hc.Interval, hc.Timeout, hc.Healthy, hc.Unhealthy = "1h", "2s", 1, 1
	pool := Pool{Backends: []Backend{{Address: source}}, HealthCheck: hc, Transport: tr, UpstreamHost: HostValue("virtual.example")}
	cfg := mustResolve(t, Config{Listeners: Listeners{HTTP(":0")}, Upstreams: Upstreams{"secure": pool}, Routes: Routes{Match("/*").ProxyTo("secure")}})
	ph, err := newPoolHandler(cfg.Upstreams["secure"])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ph.transport.CloseIdleConnections)
	return ph
}

func probeRedirectPool(ph *poolHandler) bool {
	run := &healthRun{checker: ph.hc, successes: map[*backendState]int{}, failures: map[*backendState]int{}}
	run.active.Store(true)
	run.probe(context.Background(), ph.primary[0])
	return ph.primary[0].isHealthy()
}

func TestHealthRedirectBoundary(t *testing.T) {
	pki := makeClientAuthPKI(t)
	var destinationConnections, authenticated, sourceRequests, sourceAuthenticated atomic.Int64
	destination := healthRedirectTLS(t, pki, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.VerifiedChains) > 0 {
			authenticated.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}), &destinationConnections)
	var sourceURL string
	source := healthRedirectTLS(t, pki, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceRequests.Add(1)
		if len(r.TLS.VerifiedChains) > 0 {
			sourceAuthenticated.Add(1)
		}
		if r.URL.Path == "/ok" {
			w.WriteHeader(http.StatusOK)
			return
		}
		targets := map[string]string{
			"/relative": "/ok", "/absolute": sourceURL + "/ok", "/off-origin": destination.URL,
			"/network": strings.TrimPrefix(destination.URL, "https:"), "/chain": "/off-origin",
			"/scheme": "http:" + strings.TrimPrefix(sourceURL, "https:") + "/ok", "/loop": "/loop",
		}
		http.Redirect(w, r, targets[r.URL.Path], http.StatusFound)
	}), nil)
	sourceURL = source.URL
	for _, tc := range []struct {
		name, path, host                      string
		statuses                              []int
		identity, healthy, reachesDestination bool
	}{
		{"relative", "/relative", "", nil, true, true, false},
		{"absolute", "/absolute", "", nil, true, true, false},
		{"different port", "/off-origin", "", nil, true, false, false},
		{"network path", "/network", "", nil, true, false, false},
		{"multi hop", "/chain", "", nil, true, false, false},
		{"different scheme", "/scheme", "", nil, true, false, false},
		{"loop", "/loop", "", nil, true, false, false},
		{"explicit host", "/off-origin", "probe.example", nil, true, true, false},
		{"accept redirect status", "/off-origin", "", []int{302}, true, true, false},
		{"reject redirect status", "/off-origin", "", []int{200}, true, false, false},
		{"no identity same origin", "/relative", "", nil, false, true, false},
		{"no identity off origin", "/off-origin", "", nil, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, requests, identities := destinationConnections.Load(), sourceRequests.Load(), sourceAuthenticated.Load()
			ph := healthRedirectPool(t, pki, source.URL, HealthCheck{Path: tc.path, Host: tc.host, Statuses: tc.statuses}, tc.identity)
			if got := probeRedirectPool(ph); got != tc.healthy {
				t.Errorf("healthy=%t, want %t", got, tc.healthy)
			}
			if reached := destinationConnections.Load() > before; reached != tc.reachesDestination {
				t.Errorf("destination connected=%t, want %t", reached, tc.reachesDestination)
			}
			if tc.name == "loop" && sourceRequests.Load()-requests != 10 {
				t.Errorf("loop made %d requests, want 10", sourceRequests.Load()-requests)
			}
			assertHealthProbeIdentity(t, tc.identity, sourceRequests.Load()-requests, sourceAuthenticated.Load()-identities)
		})
	}
	if authenticated.Load() != 0 {
		t.Fatal("redirect destination received the pool's client identity")
	}
}

func assertHealthProbeIdentity(t *testing.T, expected bool, requests, identities int64) {
	t.Helper()
	if expected && identities != requests {
		t.Error("selected origin did not receive its configured client identity on every hop")
	}
}

func TestHealthMTLSRedirectFromHTTP(t *testing.T) {
	pki := makeClientAuthPKI(t)
	var connections atomic.Int64
	destination := healthRedirectTLS(t, pki, noContentHandler, &connections)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusFound)
	}))
	t.Cleanup(source.Close)
	ph := healthRedirectPool(t, pki, source.URL, HealthCheck{Path: "/health"}, true)
	if probeRedirectPool(ph) || connections.Load() != 0 {
		t.Fatal("HTTP probe escaped to a TLS destination with pool identity")
	}
}

func TestHealthRedirectOriginComparison(t *testing.T) {
	first, err := idna.Lookup.ToASCII("σ.example")
	if err != nil {
		t.Fatal(err)
	}
	second, err := idna.Lookup.ToASCII("ς.example")
	if err != nil || first == second {
		t.Fatalf("test origins must have distinct transport destinations: %q %q %v", first, second, err)
	}
	for _, tc := range []struct {
		from, to string
		allowed  bool
	}{
		{"https://σ.example/a", "https://ς.example/b", false},
		{"https://σ.example/a", "https://σ.example/b", true},
		{"https://EXAMPLE.com:443/a", "https://example.COM:443/b", true},
		{"https://one.example/a", "https://two.example/b", false},
		{"https://[2001:db8::a]:8443/a", "https://[2001:db8::A]:8443/b", true},
		{"https://[fe80::1%25ETH0]/a", "https://[fe80::1%25eth0]/b", false},
		{"https://[fe80::1%25eth0]/a", "https://[fe80::1%25eth0]/b", true},
		{"https://one.example:443/a", "https://one.example:8443/b", false},
	} {
		from := httptest.NewRequest(http.MethodGet, tc.from, nil)
		to := httptest.NewRequest(http.MethodGet, tc.to, nil)
		if err := sameOriginHealthRedirect(to, []*http.Request{from}); (err == nil) != tc.allowed {
			t.Errorf("%s -> %s: error=%v, allowed=%t", tc.from, tc.to, err, tc.allowed)
		}
	}
}
