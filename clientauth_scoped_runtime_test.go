package statute

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"

	"statute.kjanat.dev/resolved"
)

func scopedClientAuthConfig(t *testing.T, pki clientAuthPKI, opts ...ListenerOption) (*server, *resolved.Listener) {
	t.Helper()
	opts = append([]ListenerOption{StaticTLS(pki.serverCertFile, pki.serverKeyFile)}, opts...)
	r := mustResolve(t, tlsRouterConfig(opts...))
	srv, err := newServer(r)
	if err != nil {
		t.Fatal(err)
	}
	return srv, r.Listeners[0]
}

func TestScopedClientAuthHandshakesAndIsolation(t *testing.T) {
	t.Parallel()
	pki := makeClientAuthPKI(t)
	other := makeClientAuthPKI(t)
	srv, l := scopedClientAuthConfig(t, pki,
		ClientAuthFor([]string{"one.example"}, ClientAuth{Mode: RequireAndVerifyClientCert, CAFiles: []string{pki.caFile}}),
		ClientAuthFor([]string{"two.example"}, ClientAuth{Mode: RequireAndVerifyClientCert, CAFiles: []string{other.caFile}}),
	)
	base := certRouterTLSConfig(srv.certRouters[l.Addr], l)
	for _, tc := range []struct {
		host string
		cert *tls.Certificate
		want bool
	}{
		{"x.example", nil, true},
		{"one.example", nil, false},
		{"one.example", &pki.clientCert, true},
		{"one.example", &pki.untrustedClientCert, false},
		{"one.example", &other.clientCert, false},
		{"two.example", &other.clientCert, true},
		{"two.example", &pki.clientCert, false},
	} {
		assertTLSHandshakeForHost(t, base, pki, tc.cert, tc.host, tc.want)
	}
	// Concurrent selections must not mutate the default or another binding's CA pool.
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			assertTLSHandshakeForHost(t, base, pki, &pki.clientCert, "one.example", true)
			assertTLSHandshakeForHost(t, base, pki, nil, "x.example", true)
		})
	}
	wg.Wait()
	if base.ClientAuth != tls.NoClientCert || base.ClientCAs != nil {
		t.Fatal("handshake mutated base client auth")
	}
}

func TestScopedClientAuthDefaultOverrideAndNoSNI(t *testing.T) {
	t.Parallel()
	pki := makeClientAuthPKI(t)
	srv, l := scopedClientAuthConfig(t, pki,
		ClientAuth{Mode: RequireAndVerifyClientCert, CAFiles: []string{pki.caFile}},
		ClientAuthFor([]string{"one.example"}, ClientAuth{Mode: RequestClientCert}),
	)
	base := certRouterTLSConfig(srv.certRouters[l.Addr], l)
	assertTLSHandshakeForHost(t, base, pki, nil, "one.example", true)
	assertTLSHandshakeForHost(t, base, pki, nil, "two.example", false)
	for _, host := range []string{"", "unknown.example"} {
		selected, err := base.GetConfigForClient(&tls.ClientHelloInfo{ServerName: host})
		if err != nil || selected.ClientAuth != tls.RequireAndVerifyClientCert || selected.ClientCAs == nil {
			t.Errorf("fallback %q: cfg=%v err=%v", host, selected, err)
		}
	}
	noSNI := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true} //nolint:gosec // G402: exercise absent SNI against the hermetic fallback certificate
	if err := exchangeScopedSession(t, base, noSNI, false); err == nil {
		t.Fatal("absent SNI bypassed requiring listener default")
	}
	noSNI.Certificates = []tls.Certificate{pki.clientCert}
	if err := exchangeScopedSession(t, base, noSNI, false); err != nil {
		t.Fatalf("authenticated absent SNI with fallback certificate: %v", err)
	}
}

func TestScopedClientAuthUnicodeTrailingSeparator(t *testing.T) {
	t.Parallel()
	pki := makeClientAuthPKI(t)
	for _, suffix := range []string{"\u3002", "\uff0e", "\uff61", "\u3002\uff0e.\uff61.."} {
		srv, l := scopedClientAuthConfig(t, pki, ClientAuthFor([]string{"one.example" + suffix}, ClientAuth{Mode: RequireAndVerifyClientCert, CAFiles: []string{pki.caFile}}))
		base := certRouterTLSConfig(srv.certRouters[l.Addr], l)
		assertTLSHandshakeForHost(t, base, pki, nil, "one.example", false)
		assertTLSHandshakeForHost(t, base, pki, &pki.untrustedClientCert, "one.example", false)
		assertTLSHandshakeForHost(t, base, pki, &pki.clientCert, "one.example", true)
	}
}

func TestScopedClientAuthSelectedCertificateFailure(t *testing.T) {
	t.Parallel()
	pki := makeClientAuthPKI(t)
	srv, l := scopedClientAuthConfig(t, pki, ClientAuthFor([]string{"one.example"}, ClientAuth{Mode: RequireAndVerifyClientCert, CAFiles: []string{pki.caFile}}))
	cr := srv.certRouters[l.Addr]
	cr.exact["one.example"] = func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		return nil, errors.New("selected source unavailable")
	}
	base := certRouterTLSConfig(cr, l)
	assertTLSHandshakeForHost(t, base, pki, &pki.clientCert, "one.example", false)
	assertTLSHandshakeForHost(t, base, pki, nil, "x.example", true)
}

func TestScopedClientAuthMaterialFailsConstruction(t *testing.T) {
	t.Parallel()
	pki := makeClientAuthPKI(t)
	dir := t.TempDir()
	writeFile(t, dir, "junk.pem", "not a certificate")
	for _, path := range []string{dir + "/missing.pem", dir + "/junk.pem"} {
		r := mustResolve(t, tlsRouterConfig(StaticTLS(pki.serverCertFile, pki.serverKeyFile), ClientAuthFor([]string{"uncovered.example"}, ClientAuth{Mode: RequireAndVerifyClientCert, CAFiles: []string{path}})))
		if _, err := newServer(r); err == nil || !strings.Contains(err.Error(), "client_auth_policies[0]") {
			t.Errorf("CA %q: %v", path, err)
		}
	}
	r := mustResolve(t, tlsRouterConfig(StaticTLS(pki.serverCertFile, pki.serverKeyFile), ClientAuthFor([]string{"one.example"}, ClientAuth{Mode: RequestClientCert})))
	r.Listeners[0].ClientAuthPolicies = append(r.Listeners[0].ClientAuthPolicies, &resolved.ClientAuthPolicy{Hosts: []string{"ONE.EXAMPLE."}, Policy: resolved.ClientAuth{Mode: resolved.ClientAuthRequest}})
	if _, err := newServer(r); err == nil {
		t.Error("hand-built duplicate binding was overwritten")
	}
}

func TestScopedClientAuthALPNAndTLSPolicy(t *testing.T) {
	t.Parallel()
	pki := makeClientAuthPKI(t)
	for _, requiringDefault := range []bool{false, true} {
		opts := []ListenerOption{HTTP2(), HTTP3(":443/udp"), AutoTLS("auto.example").Email("ops@example.test").Storage(t.TempDir()), TLSPolicy{MinVersion: TLS13}, ClientAuthFor([]string{"one.example"}, ClientAuth{Mode: RequireAndVerifyClientCert, CAFiles: []string{pki.caFile}})}
		if requiringDefault {
			opts = append(opts, ClientAuth{Mode: RequireAndVerifyClientCert, CAFiles: []string{pki.caFile}}, ClientAuthFor([]string{"x.example"}, ClientAuth{Mode: RequestClientCert}))
		}
		cfg := withPlainHTTP(tlsRouterConfig(append([]ListenerOption{StaticTLS(pki.serverCertFile, pki.serverKeyFile)}, opts...)...))
		r := mustResolve(t, cfg)
		srv, err := newServer(r)
		if err != nil {
			t.Fatal(err)
		}
		for _, base := range []*tls.Config{certRouterTLSConfig(srv.certRouters[":443"], r.Listeners[0]), srv.http3Servers[0].srv.TLSConfig} {
			assertScopedClientAuthALPN(t, base)
		}
	}
}

func assertScopedClientAuthALPN(t *testing.T, base *tls.Config) {
	t.Helper()
	for _, host := range []string{"one.example", "x.example"} {
		selected, err := base.GetConfigForClient(&tls.ClientHelloInfo{ServerName: host})
		if err != nil {
			t.Fatal(err)
		}
		if selected.MinVersion != tls.VersionTLS13 || selected.GetConfigForClient != nil {
			t.Error("lost TLS policy or kept recursive callback")
		}
		if slices.Contains(base.NextProtos, alpnHTTP3) {
			if !slices.Equal(selected.NextProtos, []string{alpnHTTP3}) {
				t.Errorf("HTTP3 ALPN = %v", selected.NextProtos)
			}
		} else if !slices.Contains(selected.NextProtos, alpnHTTP2) || slices.Contains(selected.NextProtos, alpnACMETLS) != (host == "x.example") {
			t.Errorf("%q TCP ALPN = %v", host, selected.NextProtos)
		}
	}
}

func TestScopedClientAuthServesProtocolsAndGuardsReuse(t *testing.T) {
	t.Parallel()
	pki := makeClientAuthPKI(t)
	tcpAddr, udpAddr := reserveAddr(t), reserveUDPAddr(t)
	var logs bytes.Buffer
	var logMu sync.Mutex
	cfg := Config{
		Listeners: Listeners{HTTPS(tcpAddr, StaticTLS(pki.serverCertFile, pki.serverKeyFile), HTTP2(), HTTP3(udpAddr+"/udp"),
			ClientAuthFor([]string{"one.example", "two.example"}, ClientAuth{Mode: RequireAndVerifyClientCert, CAFiles: []string{pki.caFile}}))},
		Routes:        Routes{Match("/*").Handle(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))},
		Observability: Observability{AccessLog: JSONLog(LogWriter{w: &muWriter{Mutex: &logMu, w: &logs}, name: "test"})},
	}
	srv, err := newServer(mustResolve(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := srv.Shutdown(); err != nil {
			t.Error(err)
		}
	})
	for _, proto := range []string{"h1", "h2", "h3"} {
		for _, identity := range []string{"public", "valid", "missing", "untrusted"} {
			t.Run(proto+"/"+identity, func(t *testing.T) {
				assertScopedProtocolIdentity(t, proto, identity, pki, tcpAddr, udpAddr)
			})
		}
	}
	logMu.Lock()
	defer logMu.Unlock()
	if !strings.Contains(logs.String(), `"status":421`) {
		t.Error("policy denial missing from listener access log")
	}
}

func assertScopedProtocolIdentity(t *testing.T, proto, identity string, pki clientAuthPKI, tcpAddr, udpAddr string) {
	t.Helper()
	tlsCfg := scopedClientIdentity(pki, identity)
	host := tlsCfg.ServerName
	client, addr := scopedProtocolClient(t, proto, tlsCfg, tcpAddr, udpAddr)
	for i, authority := range []string{host, "two.example", host} {
		var reused bool
		trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }}
		ctx := httptrace.WithClientTrace(t.Context(), trace)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+addr+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = authority
		resp, err := client.Do(req)
		if identity == "missing" || identity == "untrusted" {
			assertScopedTLSRejection(t, resp, err)
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 && !reused {
			t.Errorf("authority %q did not reuse the established connection", authority)
		}
		want := http.StatusNoContent
		if identity == "public" && authority != host {
			want = http.StatusMisdirectedRequest
		}
		assertScopedHTTPResponse(t, resp, proto, authority, want)
	}
}

func scopedClientIdentity(pki clientAuthPKI, identity string) *tls.Config {
	cfg := &tls.Config{RootCAs: pki.serverRoots, MinVersion: tls.VersionTLS12, ServerName: "one.example"}
	switch identity {
	case "public":
		cfg.ServerName = "x.example"
	case "valid":
		cfg.Certificates = []tls.Certificate{pki.clientCert}
	case "untrusted":
		cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &pki.untrustedClientCert, nil }
	}
	return cfg
}

func assertScopedTLSRejection(t *testing.T, resp *http.Response, err error) {
	t.Helper()
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatal("unauthenticated protected request reached HTTP")
	}
	if err == nil {
		t.Fatal("missing TLS rejection")
	}
}

func assertScopedHTTPResponse(t *testing.T, resp *http.Response, proto, authority string, want int) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != want {
		t.Errorf("authority %q: status %d, want %d", authority, resp.StatusCode, want)
	}
	if (proto == "h2" && resp.ProtoMajor != 2) || (proto == "h3" && resp.ProtoMajor != 3) {
		t.Errorf("negotiated %s for %s", resp.Proto, proto)
	}
	if resp.TLS == nil {
		t.Fatal("missing connection state")
	}
}

func scopedProtocolClient(t *testing.T, proto string, cfg *tls.Config, tcpAddr, udpAddr string) (*http.Client, string) {
	t.Helper()
	if proto == "h3" {
		tr := &http3.Transport{TLSClientConfig: cfg}
		t.Cleanup(func() { _ = tr.Close() })
		return &http.Client{Transport: tr, Timeout: 3 * time.Second}, udpAddr
	}
	tr := &http.Transport{TLSClientConfig: cfg, ForceAttemptHTTP2: proto == "h2"}
	if proto == "h1" {
		tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 3 * time.Second}, tcpAddr
}

// sharedTicketCache deliberately ignores the client's SNI key to exercise the
// server's policy checks when an adversarial peer reuses another name's ticket.
type sharedTicketCache struct{ state *tls.ClientSessionState }

func (c *sharedTicketCache) Get(string) (*tls.ClientSessionState, bool) {
	return c.state, c.state != nil
}
func (c *sharedTicketCache) Put(_ string, state *tls.ClientSessionState) { c.state = state }

func TestScopedClientAuthResumption(t *testing.T) {
	t.Parallel()
	pki, other := makeClientAuthPKI(t), makeClientAuthPKI(t)
	srv, l := scopedClientAuthConfig(t, pki,
		ClientAuthFor([]string{"one.example"}, ClientAuth{Mode: RequireAndVerifyClientCert, CAFiles: []string{pki.caFile}}),
		ClientAuthFor([]string{"two.example"}, ClientAuth{Mode: RequireAndVerifyClientCert, CAFiles: []string{other.caFile}}),
	)
	base := certRouterTLSConfig(srv.certRouters[l.Addr], l)
	for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		for _, source := range []string{"x.example", "one.example"} {
			cache := &sharedTicketCache{}
			client := &tls.Config{ServerName: source, RootCAs: pki.serverRoots, MinVersion: version, MaxVersion: version, ClientSessionCache: cache}
			if source == "one.example" {
				client.Certificates = []tls.Certificate{pki.clientCert}
			}
			for attempt := range 2 {
				if err := exchangeScopedSession(t, base, client, attempt == 1); err != nil {
					t.Fatal(err)
				}
			}
			if cache.state == nil {
				t.Fatal("server issued no session ticket")
			}
			client.ServerName = "two.example"
			if err := exchangeScopedSession(t, base, client, false); err == nil {
				t.Fatalf("TLS %x %q ticket crossed client CA boundary", version, source)
			}
		}
	}
}

func exchangeScopedSession(t *testing.T, base, cfg *tls.Config, wantResumed bool) error {
	t.Helper()
	serverSide, clientSide := bufferedConnPair(t)
	defer serverSide.Close()
	defer clientSide.Close()
	_ = serverSide.SetDeadline(time.Now().Add(3 * time.Second))
	_ = clientSide.SetDeadline(time.Now().Add(3 * time.Second))
	server, client := tls.Server(serverSide, base), tls.Client(clientSide, cfg)
	done := make(chan error, 1)
	go func() {
		err := server.HandshakeContext(context.Background())
		if err == nil {
			_, err = server.Write([]byte{1})
		}
		done <- err
	}()
	err := client.HandshakeContext(t.Context())
	if err == nil {
		var data [1]byte
		_, err = io.ReadFull(client, data[:])
	}
	serverErr := <-done
	if err != nil {
		return err
	}
	if serverErr != nil {
		return serverErr
	}
	if client.ConnectionState().DidResume != wantResumed {
		t.Errorf("DidResume=%v, want %v", client.ConnectionState().DidResume, wantResumed)
	}
	return nil
}
