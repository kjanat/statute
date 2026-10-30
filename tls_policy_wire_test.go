package statute

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

func TestScopedTLSBoundsHandshakeAndHostAdmission(t *testing.T) {
	cert, key := writeSelfSignedCert(t, "modern.example", "legacy.example")
	cfg := Config{Listeners: Listeners{HTTPS("127.0.0.1:0", StaticTLS(cert, key), TLSPolicy{MaxVersion: TLS12, StrictSNI: true}, TLSPolicyFor([]string{"modern.example"}, TLSPolicy{MinVersion: TLS13, StrictSNI: true}))}, Routes: Routes{Match("/*").Handle(statusHandler(http.StatusNoContent))}}
	srv, err := newServer(mustResolve(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown() })
	// #nosec G304 -- Read the public certificate produced by writeSelfSignedCert.
	data, err := os.ReadFile(cert)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(data) {
		t.Fatal("invalid test certificate")
	}
	address := srv.run.listeners.http[0].listener.Addr().String()
	for _, tc := range []struct {
		sni, host string
		version   uint16
		status    int
	}{
		{"modern.example", "modern.example", tls.VersionTLS13, 204},
		{"modern.example", "legacy.example", tls.VersionTLS13, 421},
		{"legacy.example", "legacy.example", tls.VersionTLS12, 204},
		{"legacy.example", "modern.example", tls.VersionTLS12, 421},
		{"modern.example", "modern.example", tls.VersionTLS12, 0},
		{"legacy.example", "legacy.example", tls.VersionTLS13, 0},
		{"uncovered.example", "uncovered.example", tls.VersionTLS12, 0},
	} {
		assertTLSPolicyRequest(t, address, roots, tc.sni, tc.host, tc.version, tc.status)
	}
}

func assertTLSPolicyRequest(t *testing.T, address string, roots *x509.CertPool, sni, host string, version uint16, status int) {
	t.Helper()
	connection, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", address, &tls.Config{RootCAs: roots, ServerName: sni, MinVersion: version, MaxVersion: version})
	if status == 0 {
		if err == nil {
			_ = connection.Close()
			t.Fatalf("unexpected handshake success for %s using TLS %x", sni, version)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(time.Second))
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+host+"/", nil)
	if err := request.Write(connection); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != status {
		t.Fatalf("SNI=%s Host=%s status=%d want=%d", sni, host, response.StatusCode, status)
	}
}

func TestScopedTLSPolicyRejectsInvalidAssignments(t *testing.T) {
	for _, option := range []ListenerOption{
		TLSPolicyFor(nil, TLSPolicy{}), TLSPolicyFor([]string{"bad/name"}, TLSPolicy{}), TLSPolicyFor([]string{"*.*.example"}, TLSPolicy{}), TLSPolicyFor([]string{"a.example", "a.example"}, TLSPolicy{}),
		TLSPolicyFor([]string{"a.example"}, TLSPolicy{MinVersion: TLS13, MaxVersion: TLS12}), TLSPolicyFor([]string{"a.example"}, TLSPolicy{CipherSuites: []CipherSuite{0xffff}}),
	} {
		cfg := Config{Listeners: Listeners{HTTPS(":8443", StaticTLS("cert.pem", "key.pem"), option)}, Routes: Routes{Match("/*").Handle(http.NotFoundHandler())}}
		if _, err := Resolve(cfg); err == nil {
			t.Fatal("invalid TLS assignment accepted")
		}
	}
}

func TestScopedNoClientCertificateOverridesRequiredDefault(t *testing.T) {
	cert, key := writeSelfSignedCert(t, "modern.example", "legacy.example")
	cfg := Config{Listeners: Listeners{HTTPS("127.0.0.1:0", StaticTLS(cert, key), ClientAuth{Mode: RequireAnyClientCert}, ClientAuthFor([]string{"modern.example"}, ClientAuth{Mode: NoClientCert}))}, Routes: Routes{Match("/*").Handle(statusHandler(http.StatusNoContent))}}
	srv, err := newServer(mustResolve(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown() })
	roots := testCertificateRoots(t, cert)
	for _, host := range []string{"modern.example", "legacy.example"} {
		transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: host, MinVersion: tls.VersionTLS12}}
		client := &http.Client{Transport: transport, Timeout: time.Second}
		t.Cleanup(client.CloseIdleConnections)
		request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+srv.run.listeners.http[0].listener.Addr().String()+"/", nil)
		request.Host = host
		response, err := client.Do(request)
		if host == "legacy.example" {
			if response != nil {
				_ = response.Body.Close()
			}
			if err == nil {
				t.Fatal("listener default stopped requiring a certificate")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			t.Fatal(response.StatusCode)
		}
	}
}

func testCertificateRoots(t *testing.T, path string) *x509.CertPool {
	t.Helper()
	// #nosec G304 -- Read a public certificate produced by this test.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(data) {
		t.Fatal("invalid test certificate")
	}
	return roots
}
