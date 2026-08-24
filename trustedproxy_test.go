package statute

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"maps"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"statute.kjanat.dev/resolved"
)

// trustedListener resolves the issue's example listener shape and returns
// the resolved form.
func trustedListener(t *testing.T, tp *TrustedProxyConfig) *resolved.Listener {
	t.Helper()
	r := mustResolve(t, Config{
		Listeners: Listeners{
			HTTPS(":443", StaticTLS("cert.pem", "key.pem"), tp),
		},
		Routes: Routes{Match("/*").Serve("./public")},
	})
	return r.Listeners[0]
}

// TestResolveTrustedProxy — CIDRs are canonicalised, the header defaults to
// X-Forwarded-For, and a configured header is stored in canonical form.
func TestResolveTrustedProxy(t *testing.T) {
	t.Parallel()
	l := trustedListener(t, TrustedProxy("203.0.113.7/24"))
	if len(l.TrustedProxies) != 1 || l.TrustedProxies[0] != "203.0.113.0/24" {
		t.Errorf("canonical CIDRs: %v", l.TrustedProxies)
	}
	if l.ClientIPHeader != "X-Forwarded-For" {
		t.Errorf("default header: %q", l.ClientIPHeader)
	}

	l = trustedListener(t, TrustedProxy("2001:db8::/32").ClientIPHeader("cf-connecting-ip"))
	if l.ClientIPHeader != "Cf-Connecting-Ip" {
		t.Errorf("canonical header: %q", l.ClientIPHeader)
	}

	untouched := trustedListener(t, nil)
	if untouched.TrustedProxies != nil || untouched.ClientIPHeader != "" {
		t.Errorf("listener without the option: %+v", untouched)
	}
}

// TestResolveTrustedProxyErrors — bad declarations fail at resolve time.
func TestResolveTrustedProxyErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		tp      *TrustedProxyConfig
		wantErr string
	}{
		{"no cidrs", TrustedProxy(), "at least one CIDR"},
		{"bad cidr", TrustedProxy("not-a-cidr"), "trusted_proxy"},
		{"bad header", TrustedProxy("10.0.0.0/8").ClientIPHeader("X Bad"), "invalid character"},
		{"explicitly empty header", TrustedProxy("10.0.0.0/8").ClientIPHeader(""), "header name: empty"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := Resolve(Config{
				Listeners: Listeners{HTTPS(":443", StaticTLS("c.pem", "k.pem"), c.tp)},
				Routes:    Routes{Match("/*").Serve("./public")},
			})
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("got %v, want substring %q", err, c.wantErr)
			}
		})
	}
}

// TestTrustedProxyClientIP — the policy decides per peer: a trusted peer
// speaks for its client through the forwarded header, everyone else is
// their own client and their headers are ignored.
func TestTrustedProxyClientIP(t *testing.T) {
	t.Parallel()
	l := &resolved.Listener{
		TrustedProxies: []string{"203.0.113.0/24"},
		ClientIPHeader: "X-Forwarded-For",
	}
	cases := []struct {
		name    string
		remote  string
		headers map[string][]string
		want    string
	}{
		{
			"trusted peer asserts the client",
			"203.0.113.7:4321",
			map[string][]string{"X-Forwarded-For": {"198.51.100.9"}},
			"198.51.100.9",
		},
		{
			"last value wins over a client-forged prefix",
			"203.0.113.7:4321",
			map[string][]string{"X-Forwarded-For": {"6.6.6.6, 198.51.100.9"}},
			"198.51.100.9",
		},
		{
			"last header line wins",
			"203.0.113.7:4321",
			map[string][]string{"X-Forwarded-For": {"6.6.6.6", "198.51.100.9"}},
			"198.51.100.9",
		},
		{
			"trusted peer without the header is itself the client",
			"203.0.113.7:4321",
			nil,
			"203.0.113.7",
		},
		{
			"trusted peer with an empty value falls back to the peer",
			"203.0.113.7:4321",
			map[string][]string{"X-Forwarded-For": {"  "}},
			"203.0.113.7",
		},
		{
			"untrusted peer cannot spoof through the header",
			"198.51.100.66:4321",
			map[string][]string{"X-Forwarded-For": {"10.0.0.1"}},
			"198.51.100.66",
		},
		{
			"peer fallbacks are bare addresses, not host:port",
			"[::ffff:198.51.100.66]:4321",
			map[string][]string{"X-Forwarded-For": {"10.0.0.1"}},
			"198.51.100.66",
		},
		{
			"ipv4-mapped ipv6 peer matches an ipv4 range",
			"[::ffff:203.0.113.7]:4321",
			map[string][]string{"X-Forwarded-For": {"198.51.100.9"}},
			"198.51.100.9",
		},
		{
			"unparsable peer stays as-is",
			"pipe",
			map[string][]string{"X-Forwarded-For": {"10.0.0.1"}},
			"pipe",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var got string
			h := trustedProxyMiddleware(l, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got = clientIP(r)
			}))
			req := httptest.NewRequest("GET", "http://x/", nil)
			req.RemoteAddr = c.remote
			maps.Copy(req.Header, c.headers)
			h.ServeHTTP(httptest.NewRecorder(), req)
			if got != c.want {
				t.Errorf("clientIP: got %q, want %q", got, c.want)
			}
		})
	}
}

// TestTrustedProxyGovernsAlone — with a peer-trust policy on the listener,
// the blanket Cloudflare and X-Forwarded-For fallbacks in clientIP must not
// resurrect a header the policy refused for an untrusted peer.
func TestTrustedProxyGovernsAlone(t *testing.T) {
	t.Parallel()
	l := &resolved.Listener{
		TrustedProxies: []string{"203.0.113.0/24"},
		ClientIPHeader: "Cf-Connecting-Ip",
	}
	var got string
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = clientIP(r)
	})
	// BehindCloudflare tags inside the policy, as buildListenerHandler wraps.
	h := trustedProxyMiddleware(l, behindCloudflareMiddleware(inner))

	req := httptest.NewRequest("GET", "http://x/", nil)
	req.RemoteAddr = "198.51.100.66:4321"
	req.Header.Set("CF-Connecting-IP", "10.0.0.1")
	req.Header.Set("X-Forwarded-For", "10.0.0.2")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got != "198.51.100.66" {
		t.Errorf("untrusted peer: got %q, want the peer address", got)
	}

	// The same headers from a trusted peer resolve through the policy's
	// configured header, not the Cloudflare pair.
	req = httptest.NewRequest("GET", "http://x/", nil)
	req.RemoteAddr = "203.0.113.7:4321"
	req.Header.Set("CF-Connecting-IP", "10.0.0.1")
	req.Header.Set("X-Forwarded-For", "10.0.0.2")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got != "10.0.0.1" {
		t.Errorf("trusted peer: got %q, want the configured header's value", got)
	}
}

// writeSelfSignedCert writes a self-signed certificate and key pair for
// the given SNI hosts into a temp dir, for listeners that need loadable
// static TLS material.
func writeSelfSignedCert(t *testing.T, hosts ...string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     hosts,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeFile(t, dir, "cert.pem", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	writeFile(t, dir, "key.pem", string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})))
	return dir + "/cert.pem", dir + "/key.pem"
}

// assertTrustedProxyEnforced sends one spoof attempt and one trusted
// assertion through the handler; the AllowIPs route behind it only admits
// the asserted client range, so the status codes prove whose address the
// policy resolved.
func assertTrustedProxyEnforced(t *testing.T, h http.Handler) {
	t.Helper()
	req := httptest.NewRequest("GET", "https://x.example/", nil)
	req.RemoteAddr = "198.51.100.66:4321"
	req.Header.Set("X-Forwarded-For", "10.0.0.1")
	if rec := runRequest(t, h, req); rec.Code != http.StatusForbidden {
		t.Errorf("spoof from untrusted peer: got %d, want 403", rec.Code)
	}

	req = httptest.NewRequest("GET", "https://x.example/", nil)
	req.RemoteAddr = "203.0.113.7:4321"
	req.Header.Set("X-Forwarded-For", "10.0.0.1")
	if rec := runRequest(t, h, req); rec.Code != http.StatusOK {
		t.Errorf("assertion from trusted peer: got %d, want 200", rec.Code)
	}
}

// TestTrustedProxyListenerWiring — the policy is enforced through the real
// listener assembly, for the TCP handler and for the handler the listener's
// HTTP/3 server received. The QUIC path once got the raw router instead of
// the wrapped listener handler, which turned the spoofing protection off
// for HTTP/3 clients only.
func TestTrustedProxyListenerWiring(t *testing.T) {
	t.Parallel()
	certFile, keyFile := writeSelfSignedCert(t, "x.example")
	staticDir := t.TempDir()
	writeFile(t, staticDir, "index.html", "ok")
	r := mustResolve(t, Config{
		Listeners: Listeners{
			HTTPS(":0",
				StaticTLS(certFile, keyFile),
				HTTP3(":0/udp"),
				TrustedProxy("203.0.113.0/24"),
			),
		},
		Routes: Routes{
			Match("/*").Serve(staticDir).With(AllowIPs("10.0.0.0/8")),
		},
	})
	srv, err := newServer(r)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	t.Cleanup(func() {
		for _, ph := range srv.pools {
			ph.transport.CloseIdleConnections()
		}
	})
	if len(srv.listeners) != 1 || len(srv.http3Servers) != 1 {
		t.Fatalf("listeners: %d tcp, %d quic; want 1 and 1", len(srv.listeners), len(srv.http3Servers))
	}

	t.Run("tcp handler", func(t *testing.T) {
		assertTrustedProxyEnforced(t, srv.listeners[0].Handler)
	})
	t.Run("http3 handler", func(t *testing.T) {
		assertTrustedProxyEnforced(t, srv.http3Servers[0].srv.Handler)
	})
}
