package statute

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"statute.kjanat.dev/resolved"
)

func TestResolveClientAuth(t *testing.T) {
	t.Parallel()
	files := []string{" /etc/statute/client-ca.pem ", "\t/etc/statute/partner-ca.pem\n"}
	wantFiles := []string{"/etc/statute/client-ca.pem", "/etc/statute/partner-ca.pem"}
	policy := ClientAuth{Mode: RequireAndVerifyClientCert, CAFiles: files}
	r := mustResolve(t, tlsRouterConfig(StaticTLS("cert.pem", "key.pem"), policy))
	got := r.Listeners[0].ClientAuth
	if got == nil {
		t.Fatal("resolved listener carries no client-auth policy")
	}
	if got.Mode != resolved.ClientAuthRequireAndVerify || !slices.Equal(got.CAFiles, wantFiles) {
		t.Errorf("resolved client auth = %+v", got)
	}
	if files[0] != " /etc/statute/client-ca.pem " {
		t.Fatal("Resolve mutated the surface CA files")
	}
	files[0] = "changed"
	if got.CAFiles[0] != "/etc/statute/client-ca.pem" {
		t.Fatal("resolved CA files alias the surface slice")
	}
}

func TestResolveClientAuthModes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		mode  ClientAuthMode
		files []string
		want  resolved.ClientAuthMode
	}{
		{RequestClientCert, nil, resolved.ClientAuthRequest},
		{RequireAnyClientCert, nil, resolved.ClientAuthRequireAny},
		{VerifyClientCertIfGiven, []string{"ca.pem"}, resolved.ClientAuthVerifyIfGiven},
		{RequireAndVerifyClientCert, []string{"ca.pem"}, resolved.ClientAuthRequireAndVerify},
	}
	for _, tc := range cases {
		t.Run(string(tc.want), func(t *testing.T) {
			t.Parallel()
			r := mustResolve(t, tlsRouterConfig(
				StaticTLS("cert.pem", "key.pem"),
				ClientAuth{Mode: tc.mode, CAFiles: tc.files},
			))
			if got := r.Listeners[0].ClientAuth.Mode; got != tc.want {
				t.Errorf("mode = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveClientAuthErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			"second policy",
			tlsRouterConfig(StaticTLS("cert.pem", "key.pem"),
				ClientAuth{Mode: RequestClientCert},
				ClientAuth{Mode: RequestClientCert}),
			"one policy allowed",
		},
		{
			"redirect",
			func() Config {
				cfg := tlsRouterConfig(StaticTLS("cert.pem", "key.pem"), ClientAuth{Mode: RequestClientCert})
				cfg.Listeners[0].RedirectTo("http")
				return cfg
			}(),
			"redirect-only listener",
		},
		{
			"missing mode",
			tlsRouterConfig(StaticTLS("cert.pem", "key.pem"), ClientAuth{CAFiles: []string{"ca.pem"}}),
			"mode is required",
		},
		{
			"unknown mode",
			tlsRouterConfig(StaticTLS("cert.pem", "key.pem"), ClientAuth{Mode: ClientAuthMode(99)}),
			"unsupported mode",
		},
		{
			"empty CA path",
			tlsRouterConfig(StaticTLS("cert.pem", "key.pem"), ClientAuth{Mode: RequestClientCert, CAFiles: []string{" "}}),
			"ca_files[0]: path is empty",
		},
		{
			"verify without CA",
			tlsRouterConfig(StaticTLS("cert.pem", "key.pem"), ClientAuth{Mode: VerifyClientCertIfGiven}),
			"requires at least one CA file",
		},
		{
			"require and verify without CA",
			tlsRouterConfig(StaticTLS("cert.pem", "key.pem"), ClientAuth{Mode: RequireAndVerifyClientCert}),
			"requires at least one CA file",
		},
		{
			"plain HTTP",
			func() Config {
				listener := HTTP(":80")
				ClientAuth{Mode: RequestClientCert}.applyListener(listener)
				return Config{
					Listeners: Listeners{listener},
					Routes:    Routes{Match("/*").Serve(".")},
				}
			}(),
			"requires an HTTPS listener",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Resolve(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestClientAuthAutomaticACMERequiresHTTPFallback(t *testing.T) {
	t.Parallel()
	auto := func(mode ClientAuthMode) Config {
		return tlsRouterConfig(
			AutoTLS("mtls.example").Email("ops@example.test").Storage("/var/lib/statute/acme"),
			ClientAuth{Mode: mode, CAFiles: []string{"client-ca.pem"}},
		)
	}
	for _, mode := range []ClientAuthMode{RequireAnyClientCert, RequireAndVerifyClientCert} {
		cfg := auto(mode)
		_, err := Resolve(cfg)
		if err == nil || !strings.Contains(err.Error(), "requires a plain HTTP listener") {
			t.Errorf("mode %d without HTTP: %v", mode, err)
		}
		cfg.Listeners = append(cfg.Listeners, HTTP(":80"))
		if _, err := Resolve(cfg); err != nil {
			t.Errorf("mode %d with HTTP: %v", mode, err)
		}
	}
	for _, mode := range []ClientAuthMode{RequestClientCert, VerifyClientCertIfGiven} {
		if _, err := Resolve(auto(mode)); err != nil {
			t.Errorf("non-requiring mode %d: %v", mode, err)
		}
	}
}

func TestClientAuthSuppressesACMETLSALPN(t *testing.T) {
	t.Parallel()
	pki := makeClientAuthPKI(t)
	cfg := tlsRouterConfig(
		AutoTLS("mtls.example").Email("ops@example.test").Storage(t.TempDir()),
		ClientAuth{Mode: RequireAndVerifyClientCert, CAFiles: []string{pki.caFile}},
	)
	cfg.Listeners = append(cfg.Listeners, HTTP(":80"))
	r := mustResolve(t, cfg)
	srv, err := newServer(r)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	cr := srv.certRouters[":443"]
	if !cr.hasACMETLS {
		t.Fatal("automatic source must remain indexed by the shared autocert manager")
	}
	if protos := certRouterTLSConfig(cr, r.Listeners[0]).NextProtos; slices.Contains(protos, "acme-tls/1") {
		t.Errorf("client-auth listener advertises unusable TLS-ALPN-01: %v", protos)
	}
}

func TestClientAuthMaterialFailsServerConstruction(t *testing.T) {
	t.Parallel()
	pki := makeClientAuthPKI(t)
	junkDir := t.TempDir()
	writeFile(t, junkDir, "junk.pem", "not a certificate")
	cases := []struct {
		name string
		path string
		want string
	}{
		{"missing", t.TempDir() + "/missing.pem", "client CA file"},
		{"not PEM", junkDir + "/junk.pem", "no certificates found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := mustResolve(t, tlsRouterConfig(
				StaticTLS(pki.serverCertFile, pki.serverKeyFile),
				ClientAuth{Mode: RequireAndVerifyClientCert, CAFiles: []string{tc.path}},
			))
			_, err := newServer(r)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("newServer error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestBuildClientCAPoolRequiresFilesForVerifyingModes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		policy  *resolved.ClientAuth
		wantErr bool
	}{
		{"unset", nil, false},
		{"request", &resolved.ClientAuth{Mode: resolved.ClientAuthRequest}, false},
		{"require any", &resolved.ClientAuth{Mode: resolved.ClientAuthRequireAny}, false},
		{"verify if given", &resolved.ClientAuth{Mode: resolved.ClientAuthVerifyIfGiven}, true},
		{"require and verify", &resolved.ClientAuth{Mode: resolved.ClientAuthRequireAndVerify}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool, err := buildClientCAPool(tc.policy)
			if (err != nil) != tc.wantErr {
				t.Fatalf("buildClientCAPool() error = %v, wantErr %v", err, tc.wantErr)
			}
			if pool != nil {
				t.Errorf("buildClientCAPool() pool = %v, want nil", pool)
			}
		})
	}
}

func TestClientAuthHandshakeModes(t *testing.T) {
	t.Parallel()
	pki := makeClientAuthPKI(t)
	cases := []struct {
		name                   string
		mode                   ClientAuthMode
		caFiles                []string
		noCert, valid, invalid bool
	}{
		{"request", RequestClientCert, nil, true, true, true},
		{"require any", RequireAnyClientCert, nil, false, true, true},
		{"verify if given", VerifyClientCertIfGiven, []string{pki.caFile}, true, true, false},
		{"require and verify", RequireAndVerifyClientCert, []string{pki.caFile}, false, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			serverCfg := clientAuthServerConfig(t, pki, tc.mode, tc.caFiles)
			assertTLSHandshake(t, serverCfg, pki, nil, tc.noCert)
			assertTLSHandshake(t, serverCfg, pki, &pki.clientCert, tc.valid)
			assertTLSHandshake(t, serverCfg, pki, &pki.untrustedClientCert, tc.invalid)
		})
	}
}

func TestClientAuthCoversEverySNISourceAndHTTP3(t *testing.T) {
	t.Parallel()
	pki := makeClientAuthPKI(t)
	r := mustResolve(t, tlsRouterConfig(
		StaticTLSFor("one.example", pki.serverCertFile, pki.serverKeyFile),
		StaticTLSFor("two.example", pki.serverCertFile, pki.serverKeyFile),
		HTTP3(":443/udp"),
		ClientAuth{Mode: RequireAndVerifyClientCert, CAFiles: []string{pki.caFile}},
	))
	srv, err := newServer(r)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	if len(srv.http3Servers) != 1 {
		t.Fatalf("HTTP/3 servers = %d, want 1", len(srv.http3Servers))
	}
	h3cfg := srv.http3Servers[0].srv.TLSConfig
	if h3cfg.ClientAuth != tls.RequireAndVerifyClientCert || h3cfg.ClientCAs == nil {
		t.Errorf("HTTP/3 client auth = %v, CAs nil=%v", h3cfg.ClientAuth, h3cfg.ClientCAs == nil)
	}
	for _, host := range []string{"one.example", "two.example"} {
		cfg := certRouterTLSConfig(srv.certRouters[":443"], r.Listeners[0])
		assertTLSHandshakeForHost(t, cfg, pki, &pki.clientCert, host, true)
	}
}

func TestApplyClientAuthUnknownModeFailsClosed(t *testing.T) {
	t.Parallel()
	cfg := &tls.Config{}
	applyClientAuth(cfg, &resolved.ClientAuth{Mode: "unknown"}, x509.NewCertPool())
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert || cfg.ClientCAs != nil {
		t.Errorf("unknown mode widened to ClientAuth=%v ClientCAs=%v", cfg.ClientAuth, cfg.ClientCAs)
	}
}

type clientAuthPKI struct {
	caFile              string
	serverCertFile      string
	serverKeyFile       string
	clientCert          tls.Certificate
	untrustedClientCert tls.Certificate
	serverRoots         *x509.CertPool
}

func makeClientAuthPKI(t *testing.T) clientAuthPKI {
	t.Helper()
	dir := t.TempDir()
	ca, caKey, caDER := makeTestCA(t, "client-auth-test-ca", 1)
	badCA, badCAKey, _ := makeTestCA(t, "untrusted-client-ca", 2)
	writeFile(t, dir, "client-ca.pem", pemCert(caDER))
	caFile := dir + "/client-ca.pem"
	serverCertFile, serverKeyFile, _ := makeSignedTestCert(t, dir, "server", 3, ca, caKey, pkix.Name{CommonName: "server"}, []string{"x.example", "one.example", "two.example"}, nil, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
	_, _, clientCert := makeSignedTestCert(t, dir, "client", 4, ca, caKey, pkix.Name{CommonName: "verified-client", Organization: []string{"Statute tests"}}, []string{"client.example"}, []string{"client@example.test"}, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
	_, _, untrusted := makeSignedTestCert(t, dir, "untrusted", 5, badCA, badCAKey, pkix.Name{CommonName: "untrusted-client"}, nil, nil, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return clientAuthPKI{
		caFile: caFile, serverCertFile: serverCertFile, serverKeyFile: serverKeyFile,
		clientCert: clientCert, untrustedClientCert: untrusted, serverRoots: pool,
	}
}

func makeTestCA(t *testing.T, name string, serial int64) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	t.Helper()
	key := newTestKey(t)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key, der
}

func makeSignedTestCert(t *testing.T, dir, name string, serial int64, ca *x509.Certificate, caKey *ecdsa.PrivateKey, subject pkix.Name, dnsNames, emails []string, usages []x509.ExtKeyUsage) (string, string, tls.Certificate) {
	t.Helper()
	key := newTestKey(t)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: subject,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: dnsNames, EmailAddresses: emails, ExtKeyUsage: usages,
		KeyUsage: x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, name+".crt", pemCert(der))
	writeFile(t, dir, name+".key", string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})))
	certFile := dir + "/" + name + ".crt"
	keyFile := dir + "/" + name + ".key"
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile, cert
}

func newTestKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func pemCert(der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func clientAuthServerConfig(t *testing.T, pki clientAuthPKI, mode ClientAuthMode, caFiles []string) *tls.Config {
	t.Helper()
	r := mustResolve(t, tlsRouterConfig(
		StaticTLS(pki.serverCertFile, pki.serverKeyFile),
		ClientAuth{Mode: mode, CAFiles: caFiles},
	))
	srv, err := newServer(r)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	return certRouterTLSConfig(srv.certRouters[":443"], r.Listeners[0])
}

func assertTLSHandshake(t *testing.T, serverCfg *tls.Config, pki clientAuthPKI, cert *tls.Certificate, want bool) {
	t.Helper()
	assertTLSHandshakeForHost(t, serverCfg, pki, cert, "x.example", want)
}

func assertTLSHandshakeForHost(t *testing.T, serverCfg *tls.Config, pki clientAuthPKI, cert *tls.Certificate, host string, want bool) {
	t.Helper()
	serverSide, clientSide := bufferedConnPair(t)
	deadline := time.Now().Add(2 * time.Second)
	_ = serverSide.SetDeadline(deadline)
	_ = clientSide.SetDeadline(deadline)
	server := tls.Server(serverSide, serverCfg.Clone())
	clientCfg := &tls.Config{ServerName: host, RootCAs: pki.serverRoots, MinVersion: tls.VersionTLS12}
	if cert != nil {
		clientCfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return cert, nil
		}
	}
	client := tls.Client(clientSide, clientCfg)
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.Handshake()
	}()
	clientErr := client.Handshake()
	srvErr := <-serverErr
	_ = client.Close()
	_ = server.Close()
	got := srvErr == nil
	if got != want {
		t.Errorf("handshake with cert=%v: got success=%v (client=%v server=%v), want %v", cert != nil, got, clientErr, srvErr, want)
	}
}

func bufferedConnPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	client, err := net.DialTimeout("tcp", listener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	server, err := listener.Accept()
	if err != nil {
		_ = client.Close()
		t.Fatalf("accept: %v", err)
	}
	return server, client
}

func TestVerifiedClientCertificateLogFields(t *testing.T) {
	t.Parallel()
	pki := makeClientAuthPKI(t)
	leaf, err := x509.ParseCertificate(pki.clientCert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	leaf.URIs = []*url.URL{{Scheme: "spiffe", Host: "example.test", Path: "/client"}}
	leaf.IPAddresses = []net.IP{net.ParseIP("192.0.2.10")}
	entry := map[string]any{}
	addVerifiedClientCert(entry, (&httpRequestWithTLS{leaf: leaf}).request())
	if entry["client_cert_subject"] != "CN=verified-client,O=Statute tests" {
		t.Errorf("subject = %v", entry["client_cert_subject"])
	}
	wantSANs := []string{"dns:client.example", "email:client@example.test", "ip:192.0.2.10", "uri:spiffe://example.test/client"}
	got, _ := entry["client_cert_sans"].([]string)
	if !slices.Equal(got, wantSANs) {
		t.Errorf("SANs = %v, want %v", got, wantSANs)
	}

	entry = map[string]any{}
	req := (&httpRequestWithTLS{leaf: leaf, unverified: true}).request()
	addVerifiedClientCert(entry, req)
	if len(entry) != 0 {
		t.Errorf("unverified certificate was logged as identity: %v", entry)
	}
}

func TestAccessLogIncludesOnlyVerifiedClientIdentity(t *testing.T) {
	t.Parallel()
	pki := makeClientAuthPKI(t)
	leaf, err := x509.ParseCertificate(pki.clientCert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		verified   bool
		wantFields bool
	}{
		{"verified", true, true},
		{"unverified", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			h := accessLogMiddleware(resolved.AccessLog{Enabled: true, Writer: &buf, SampleRate: 1}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			req := httptest.NewRequest(http.MethodGet, "https://x.example/", nil)
			req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
			if tc.verified {
				req.TLS.VerifiedChains = [][]*x509.Certificate{{leaf}}
			}
			runRequest(t, h, req)
			var entry map[string]any
			if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
				t.Fatal(err)
			}
			_, hasSubject := entry["client_cert_subject"]
			_, hasSANs := entry["client_cert_sans"]
			if hasSubject != tc.wantFields || hasSANs != tc.wantFields {
				t.Errorf("client identity fields: subject=%v sans=%v, want %v: %v", hasSubject, hasSANs, tc.wantFields, entry)
			}
		})
	}
}

type httpRequestWithTLS struct {
	leaf       *x509.Certificate
	unverified bool
}

func (f *httpRequestWithTLS) request() *http.Request {
	state := &tls.ConnectionState{PeerCertificates: []*x509.Certificate{f.leaf}}
	if !f.unverified {
		state.VerifiedChains = [][]*x509.Certificate{{f.leaf}}
	}
	return &http.Request{TLS: state}
}
