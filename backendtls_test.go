package statute

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"statute.kjanat.dev/resolved"
)

// tlsEchoBackend starts a TLS httptest server answering 200 "secure", and
// writes its self-signed certificate to a PEM file the pool can trust as a
// root. The certificate carries SANs for example.com and 127.0.0.1.
func tlsEchoBackend(t *testing.T) (srv *httptest.Server, caFile string) {
	t.Helper()
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("secure"))
	}))
	t.Cleanup(srv.Close)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	dir := t.TempDir()
	writeFile(t, dir, "ca.pem", string(pemBytes))
	return srv, dir + "/ca.pem"
}

// tlsPoolConfig routes everything to one https backend with the given
// transport policy.
func tlsPoolConfig(backendURL string, tr Transport) Config {
	return Config{
		Listeners: Listeners{HTTP(":0")},
		Upstreams: Upstreams{
			"secure": Pool{
				Backends:  []Backend{{Address: backendURL}},
				Transport: tr,
			},
		},
		Routes: Routes{Match("/*").ProxyTo("secure")},
	}
}

// proxyThrough resolves cfg, builds the server, and runs one request.
func proxyThrough(t *testing.T, cfg Config) *httptest.ResponseRecorder {
	t.Helper()
	srv, err := newServer(mustResolve(t, cfg))
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	t.Cleanup(func() {
		for _, ph := range srv.pools {
			ph.shutdown()
		}
	})
	return runRequest(t, srv.buildRouter(), httptest.NewRequest("GET", "http://x/", nil))
}

// TestProxyToTLSBackendWithRootCA — a pool trusting the backend's CA file
// proxies over https successfully, verifying against the configured
// ServerName; the same pool without the policy is refused, proving the 200
// came from verification and not from a permissive default.
func TestProxyToTLSBackendWithRootCA(t *testing.T) {
	t.Parallel()
	backend, caFile := tlsEchoBackend(t)

	rec := proxyThrough(t, tlsPoolConfig(backend.URL, Transport{
		ServerName:  "example.com",
		RootCAFiles: []string{caFile},
	}))
	if rec.Code != http.StatusOK || rec.Body.String() != "secure" {
		t.Errorf("verified proxy: got %d %q, want 200 \"secure\"", rec.Code, rec.Body.String())
	}

	rec = proxyThrough(t, tlsPoolConfig(backend.URL, Transport{}))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("unconfigured proxy: got %d, want 502 (unknown authority)", rec.Code)
	}
}

// TestProxyToTLSBackendInsecure — the explicit escape hatch connects without
// any trust configuration.
func TestProxyToTLSBackendInsecure(t *testing.T) {
	t.Parallel()
	backend, _ := tlsEchoBackend(t)
	rec := proxyThrough(t, tlsPoolConfig(backend.URL, Transport{InsecureSkipVerify: true}))
	if rec.Code != http.StatusOK {
		t.Errorf("insecure proxy: got %d, want 200", rec.Code)
	}
}

// TestHealthCheckSharesPoolTransport — the prober's client rides the exact
// transport proxy traffic uses, so one TLS policy covers both and cannot
// drift.
func TestHealthCheckSharesPoolTransport(t *testing.T) {
	t.Parallel()
	r := mustResolve(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Upstreams: Upstreams{
			"api": Pool{
				Backends:    []Backend{{Address: "127.0.0.1:9001"}},
				HealthCheck: HealthCheck{Path: "/healthz", Interval: "1h"},
			},
		},
		Routes: Routes{Match("/*").ProxyTo("api")},
	})
	ph, err := newPoolHandler(r.Upstreams["api"])
	if err != nil {
		t.Fatalf("newPoolHandler: %v", err)
	}
	t.Cleanup(ph.shutdown)
	if ph.hc.client.Transport != http.RoundTripper(ph.transport) {
		t.Error("health checker does not share the pool transport")
	}
}

// TestResolveTransportTLS — the TLS fields survive resolution, the CA list
// is copied rather than aliased, and an empty path fails at resolve time.
func TestResolveTransportTLS(t *testing.T) {
	t.Parallel()
	files := []string{"/etc/ca/internal.pem"}
	tr, err := resolveTransport(Transport{
		ServerName:         "foo.internal.example",
		RootCAFiles:        files,
		InsecureSkipVerify: true,
	})
	if err != nil {
		t.Fatalf("resolveTransport: %v", err)
	}
	if tr.ServerName != "foo.internal.example" || !tr.InsecureSkipVerify || len(tr.RootCAFiles) != 1 {
		t.Errorf("resolved transport: %+v", tr)
	}
	files[0] = "/mutated"
	if tr.RootCAFiles[0] != "/etc/ca/internal.pem" {
		t.Error("RootCAFiles aliases the caller's slice")
	}

	_, err = resolveTransport(Transport{RootCAFiles: []string{"  "}})
	if err == nil || !strings.Contains(err.Error(), "root_ca_files[0]: path is empty") {
		t.Errorf("empty CA path: got %v, want path-is-empty error", err)
	}
}

// TestBackendTLSConfig — the policy builder returns nil for a default pool,
// carries each field through, and turns unreadable or certificate-free CA
// files into errors.
func TestBackendTLSConfig(t *testing.T) {
	t.Parallel()
	if cfg, err := backendTLSConfig(resolved.Transport{}); err != nil || cfg != nil {
		t.Errorf("default transport: got %v, %v; want nil, nil", cfg, err)
	}

	_, caFile := tlsEchoBackend(t)
	cfg, err := backendTLSConfig(resolved.Transport{
		ServerName:  "example.com",
		RootCAFiles: []string{caFile},
	})
	if err != nil {
		t.Fatalf("backendTLSConfig: %v", err)
	}
	if cfg.ServerName != "example.com" || cfg.RootCAs == nil || cfg.InsecureSkipVerify {
		t.Errorf("policy: %+v", cfg)
	}

	if _, err := backendTLSConfig(resolved.Transport{RootCAFiles: []string{"/nonexistent/ca.pem"}}); err == nil {
		t.Error("missing CA file: want error")
	}

	dir := t.TempDir()
	writeFile(t, dir, "junk.pem", "not a certificate")
	_, err = backendTLSConfig(resolved.Transport{RootCAFiles: []string{dir + "/junk.pem"}})
	if err == nil || !strings.Contains(err.Error(), "no certificates found") {
		t.Errorf("junk CA file: got %v, want no-certificates error", err)
	}
}

// TestLint_TLS002OnInsecureUpstream — disabling verification is loud but not
// fatal, and a verifying pool stays quiet.
func TestLint_TLS002OnInsecureUpstream(t *testing.T) {
	t.Parallel()
	insecure := tlsPoolConfig("https://127.0.0.1:1", Transport{InsecureSkipVerify: true})
	findings, err := Lint(insecure)
	if err != nil {
		t.Fatal(err)
	}
	hit := false
	for _, f := range findings {
		if f.Code == "TLS002" {
			hit = true
			if f.Severity != SeverityWarning {
				t.Errorf("TLS002 severity: got %s, want warning", f.Severity)
			}
			if !strings.Contains(f.Path, `upstreams["secure"]`) {
				t.Errorf("TLS002 path: got %s", f.Path)
			}
		}
	}
	if !hit {
		t.Errorf("TLS002 not raised; findings: %v", findings)
	}

	verifying := tlsPoolConfig("https://127.0.0.1:1", Transport{RootCAFiles: []string{"/etc/ca.pem"}})
	findings, err = Lint(verifying)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		if f.Code == "TLS002" {
			t.Errorf("TLS002 raised for a verifying pool: %v", f)
		}
	}
}

// TestNewServerRejectsBadRootCA — a pool whose CA file cannot be read fails
// server construction with the file named, so a broken trust configuration
// is a startup error rather than a silent fallback to the system roots.
func TestNewServerRejectsBadRootCA(t *testing.T) {
	t.Parallel()
	cfg := tlsPoolConfig("https://127.0.0.1:1", Transport{RootCAFiles: []string{"/nonexistent/ca.pem"}})
	_, err := newServer(mustResolve(t, cfg))
	if err == nil || !strings.Contains(err.Error(), "/nonexistent/ca.pem") {
		t.Errorf("got %v, want a construction error naming the CA file", err)
	}
}
