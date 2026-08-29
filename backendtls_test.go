package statute

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
			ph.transport.CloseIdleConnections()
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
	t.Cleanup(ph.transport.CloseIdleConnections)
	if ph.hc.client.Transport != http.RoundTripper(ph.transport) {
		t.Error("health checker does not share the pool transport")
	}
}

// TestUpstreamClientCertificateCoversProxyAndHealth proves an mTLS backend
// accepts both traffic paths through the pool's one shared transport. Omitting
// the identity fails the proxy handshake closed.
func TestUpstreamClientCertificateCoversProxyAndHealth(t *testing.T) {
	t.Parallel()
	pki := makeClientAuthPKI(t)
	serverCert, err := tls.LoadX509KeyPair(pki.serverCertFile, pki.serverKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte("ok"))
			return
		}
		_, _ = w.Write([]byte("authenticated"))
	}))
	backend.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert}, ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs: pki.serverRoots, MinVersion: tls.VersionTLS12,
	}
	backend.StartTLS()
	t.Cleanup(backend.Close)

	pool := Pool{
		Backends: []Backend{{Address: backend.URL}},
		HealthCheck: HealthCheck{
			Path: "/health", Interval: "1h", Timeout: "1s", Healthy: 1, Unhealthy: 1,
		},
		Transport: Transport{
			ServerName: "x.example", RootCAFiles: []string{pki.caFile},
			ClientCertificate: ClientCertificate{CertFile: pki.clientCertFile, KeyFile: pki.clientKeyFile},
		},
	}
	r := mustResolve(t, Config{
		Listeners: Listeners{HTTP(":0")}, Upstreams: Upstreams{"secure": pool},
		Routes: Routes{Match("/*").ProxyTo("secure")},
	})
	ph, err := newPoolHandler(r.Upstreams["secure"])
	if err != nil {
		t.Fatalf("newPoolHandler: %v", err)
	}
	t.Cleanup(ph.transport.CloseIdleConnections)
	b := ph.primary[0]
	b.markHealthy(false)
	run := &healthRun{checker: ph.hc, successes: map[*backendState]int{}, failures: map[*backendState]int{}}
	run.active.Store(true)
	run.probe(context.Background(), b)
	if !b.isHealthy() {
		t.Error("mTLS health probe did not authenticate")
	}
	rec := runRequest(t, ph, httptest.NewRequest(http.MethodGet, "http://client/", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "authenticated" {
		t.Errorf("mTLS proxy: got %d %q, want 200 authenticated", rec.Code, rec.Body.String())
	}

	pool.Transport.ClientCertificate = ClientCertificate{}
	rec = proxyThrough(t, Config{
		Listeners: Listeners{HTTP(":0")}, Upstreams: Upstreams{"secure": pool},
		Routes: Routes{Match("/*").ProxyTo("secure")},
	})
	if rec.Code != http.StatusBadGateway {
		t.Errorf("proxy without client identity: got %d, want 502", rec.Code)
	}
}

// TestResolveTransportFlushInterval — a set flush interval resolves to its
// duration, unset stays at Go's zero default.
func TestResolveTransportFlushInterval(t *testing.T) {
	t.Parallel()
	tr, err := resolveTransport(Transport{FlushInterval: "100ms"})
	if err != nil {
		t.Fatalf("resolveTransport: %v", err)
	}
	if tr.FlushInterval != 100*time.Millisecond {
		t.Errorf("FlushInterval: got %v, want 100ms", tr.FlushInterval)
	}
	tr, err = resolveTransport(Transport{})
	if err != nil {
		t.Fatalf("resolveTransport: %v", err)
	}
	if tr.FlushInterval != 0 {
		t.Errorf("default FlushInterval: got %v, want 0", tr.FlushInterval)
	}
}

// TestResolveTransportResponseHeaderTimeout covers explicit and zero defaults.
func TestResolveTransportResponseHeaderTimeout(t *testing.T) {
	t.Parallel()
	tr, err := resolveTransport(Transport{ResponseHeaderTimeout: "5s"})
	if err != nil {
		t.Fatalf("resolveTransport: %v", err)
	}
	if tr.ResponseHeaderTimeout != 5*time.Second {
		t.Errorf("ResponseHeaderTimeout: got %v, want 5s", tr.ResponseHeaderTimeout)
	}
	tr, err = resolveTransport(Transport{})
	if err != nil {
		t.Fatalf("resolveTransport: %v", err)
	}
	if tr.ResponseHeaderTimeout != 0 {
		t.Errorf("default ResponseHeaderTimeout: got %v, want 0", tr.ResponseHeaderTimeout)
	}
}

// TestPoolTransportCarriesResponseHeaderTimeout pins transport and health timeouts.
func TestPoolTransportCarriesResponseHeaderTimeout(t *testing.T) {
	t.Parallel()
	r := mustResolve(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Upstreams: Upstreams{
			"api": Pool{
				Backends:    []Backend{{Address: "127.0.0.1:9001"}},
				HealthCheck: HealthCheck{Path: "/healthz", Interval: "1h", Timeout: "2s"},
				Transport:   Transport{ResponseHeaderTimeout: "5s"},
			},
			"plain": Pool{Backends: []Backend{{Address: "127.0.0.1:9002"}}},
		},
		Routes: Routes{
			Match("/api/*").ProxyTo("api"),
			Match("/*").ProxyTo("plain"),
		},
	})
	ph, err := newPoolHandler(r.Upstreams["api"])
	if err != nil {
		t.Fatalf("newPoolHandler: %v", err)
	}
	t.Cleanup(ph.transport.CloseIdleConnections)
	if ph.transport.ResponseHeaderTimeout != 5*time.Second {
		t.Errorf("transport ResponseHeaderTimeout: got %v, want 5s", ph.transport.ResponseHeaderTimeout)
	}
	if ph.hc.client.Timeout != 2*time.Second {
		t.Errorf("health client Timeout: got %v, want 2s", ph.hc.client.Timeout)
	}

	plain, err := newPoolHandler(r.Upstreams["plain"])
	if err != nil {
		t.Fatalf("newPoolHandler: %v", err)
	}
	t.Cleanup(plain.transport.CloseIdleConnections)
	if plain.transport.ResponseHeaderTimeout != 0 {
		t.Errorf("default ResponseHeaderTimeout: got %v, want 0", plain.transport.ResponseHeaderTimeout)
	}
}

// TestBackendProxyCarriesFlushInterval — the pool's flush interval lands on
// every backend proxy (routes sharing the pool observe one value by
// construction), a default pool stays at zero, and probes keep riding the
// pool transport when the interval is set.
func TestBackendProxyCarriesFlushInterval(t *testing.T) {
	t.Parallel()
	r := mustResolve(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Upstreams: Upstreams{
			"api": Pool{
				Backends:    []Backend{{Address: "127.0.0.1:9001"}, {Address: "127.0.0.1:9002", Backup: true}},
				HealthCheck: HealthCheck{Path: "/healthz", Interval: "1h"},
				Transport:   Transport{FlushInterval: "100ms"},
			},
			"plain": Pool{
				Backends: []Backend{{Address: "127.0.0.1:9003"}},
			},
		},
		Routes: Routes{
			Match("/a/*").ProxyTo("api"),
			Match("/b/*").ProxyTo("api"),
			Match("/*").ProxyTo("plain"),
		},
	})
	ph, err := newPoolHandler(r.Upstreams["api"])
	if err != nil {
		t.Fatalf("newPoolHandler: %v", err)
	}
	t.Cleanup(ph.transport.CloseIdleConnections)
	for i, bs := range append(append([]*backendState{}, ph.primary...), ph.backup...) {
		if bs.rp.FlushInterval != 100*time.Millisecond {
			t.Errorf("backend %d FlushInterval: got %v, want 100ms", i, bs.rp.FlushInterval)
		}
	}
	if ph.hc.client.Transport != http.RoundTripper(ph.transport) {
		t.Error("health checker does not share the pool transport")
	}

	def, err := newPoolHandler(r.Upstreams["plain"])
	if err != nil {
		t.Fatalf("newPoolHandler: %v", err)
	}
	t.Cleanup(def.transport.CloseIdleConnections)
	if def.primary[0].rp.FlushInterval != 0 {
		t.Errorf("default pool FlushInterval: got %v, want 0", def.primary[0].rp.FlushInterval)
	}
}

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

func TestResolveTransportClientCertificate(t *testing.T) {
	t.Parallel()
	surface := ClientCertificate{CertFile: " /etc/client.crt ", KeyFile: "\t/etc/client.key\n"}
	tr, err := resolveTransport(Transport{ClientCertificate: surface})
	if err != nil {
		t.Fatal(err)
	}
	if tr.ClientCertificate == nil || tr.ClientCertificate.CertFile != "/etc/client.crt" || tr.ClientCertificate.KeyFile != "/etc/client.key" {
		t.Errorf("resolved client certificate: %+v", tr.ClientCertificate)
	}
	if surface.CertFile != " /etc/client.crt " || surface.KeyFile != "\t/etc/client.key\n" {
		t.Errorf("Resolve mutated the surface client certificate: %+v", surface)
	}
	for _, clientCertificate := range []ClientCertificate{{CertFile: "/client.crt"}, {KeyFile: "/client.key"}} {
		_, err = resolveTransport(Transport{ClientCertificate: clientCertificate})
		if err == nil || !strings.Contains(err.Error(), "cert_file and key_file must be set together") {
			t.Errorf("incomplete client certificate %+v: got %v, want paired-path error", clientCertificate, err)
		}
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

func TestBackendTLSConfigClientCertificate(t *testing.T) {
	t.Parallel()
	pki := makeClientAuthPKI(t)
	cfg, err := backendTLSConfig(resolved.Transport{ClientCertificate: &resolved.ClientCertificate{
		CertFile: pki.clientCertFile, KeyFile: pki.clientKeyFile,
	}})
	if err != nil || len(cfg.Certificates) != 1 {
		t.Fatalf("client certificate: cfg=%+v err=%v", cfg, err)
	}
	_, err = backendTLSConfig(resolved.Transport{ClientCertificate: &resolved.ClientCertificate{
		CertFile: "/nonexistent/client.crt", KeyFile: "/nonexistent/client.key",
	}})
	if err == nil || !strings.Contains(err.Error(), "client certificate") {
		t.Errorf("missing client certificate: got %v, want client-certificate error", err)
	}
	_, err = backendTLSConfig(resolved.Transport{ClientCertificate: &resolved.ClientCertificate{
		CertFile: pki.clientCertFile, KeyFile: pki.serverKeyFile,
	}})
	if err == nil || !strings.Contains(err.Error(), "private key") {
		t.Errorf("mismatched client key: got %v, want private-key error", err)
	}
	dir := t.TempDir()
	writeFile(t, dir, "malformed.crt", "not a certificate")
	_, err = backendTLSConfig(resolved.Transport{ClientCertificate: &resolved.ClientCertificate{
		CertFile: dir + "/malformed.crt", KeyFile: pki.clientKeyFile,
	}})
	if err == nil || !strings.Contains(err.Error(), "client certificate") {
		t.Errorf("malformed client certificate: got %v, want client-certificate error", err)
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
