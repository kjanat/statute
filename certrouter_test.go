package statute

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"statute.kjanat.dev/resolved"
)

// tlsRouterConfig is the config scaffolding shared by the resolve-level
// tests: one HTTPS listener with the given TLS sources, one pool, one route.
func tlsRouterConfig(opts ...ListenerOption) Config {
	return Config{
		Listeners: Listeners{HTTPS(":443", opts...)},
		Upstreams: Upstreams{"a": Pool{Backends: []Backend{{Address: "127.0.0.1:1"}}}},
		Routes:    Routes{Match("/*").ProxyTo("a")},
	}
}

// withPlainHTTP appends a plain HTTP listener to cfg. An HTTP-01 source
// requires one — the challenge tokens are served on "http" listeners only,
// so resolve rejects the config without it. Appended last so the HTTPS
// listener under test stays Listeners[0].
func withPlainHTTP(cfg Config) Config {
	cfg.Listeners = append(cfg.Listeners, HTTP(":80"))
	return cfg
}

// TestResolveMultiSourceTLS — the issue #36 shape: one listener mixing an
// HTTP-01 source, a DNS-01 wildcard source, and per-host plus fallback
// static material. All four lower into the source slices in declaration
// order, and the legacy singular fields mirror the first of each kind.
func TestResolveMultiSourceTLS(t *testing.T) {
	t.Parallel()
	cfg := withPlainHTTP(tlsRouterConfig(
		AutoTLS("foo.example.com").Email("ops@example.com").Storage("/var/lib/c").HTTP01(),
		AutoTLS("*.bar.example").Email("ops@example.com").Storage("/var/lib/c").CloudflareDNS01("tok"),
		StaticTLSFor("BAZ.example.net.", "cert.pem", "key.pem"),
		StaticTLS("fb.pem", "fbk.pem"),
	))
	r := mustResolve(t, cfg)
	l := r.Listeners[0]

	if n := len(l.AutoTLSSources); n != 2 {
		t.Fatalf("auto sources: got %d, want 2", n)
	}
	assertChallengePolicies(t, l)
	if n := len(l.StaticTLSSources); n != 2 {
		t.Fatalf("static sources: got %d, want 2", n)
	}
	if got := l.StaticTLSSources[0].Host; got != "baz.example.net" {
		t.Errorf("host not canonicalised: %q", got)
	}
	if got := l.StaticTLSSources[1].Host; got != "" {
		t.Errorf("fallback host: got %q, want empty", got)
	}
	if l.AutoTLS != l.AutoTLSSources[0] || l.StaticTLS != l.StaticTLSSources[0] {
		t.Errorf("singular fields must mirror the first source of each kind")
	}
}

// assertChallengePolicies checks the two ACME sources of the multi-source
// fixture kept their declaration order and resolved challenge policies.
func assertChallengePolicies(t *testing.T, l *resolved.Listener) {
	t.Helper()
	if l.AutoTLSSources[0].DNS01 != nil || l.AutoTLSSources[1].DNS01 == nil {
		t.Errorf("challenge policies out of order: %+v", l.AutoTLSSources)
	}
	if got := l.AutoTLSSources[0].Challenge; got != resolved.ChallengeHTTP01 {
		t.Errorf("HTTP01() source challenge: got %v, want ChallengeHTTP01", got)
	}
	if got := l.AutoTLSSources[1].Challenge; got != resolved.ChallengeDNS01 {
		t.Errorf("DNS-01 source challenge: got %v, want ChallengeDNS01", got)
	}
}

func TestResolveTLSSourceErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		opts []ListenerOption
		want string
	}{
		{
			"HTTP01 and CloudflareDNS01 on one source",
			[]ListenerOption{AutoTLS("a.example").Email("x@x").Storage("/v").HTTP01().CloudflareDNS01("tok")},
			"mutually exclusive",
		},
		{
			"wildcard on HTTP-01 source",
			[]ListenerOption{AutoTLS("*.a.example").Email("x@x").Storage("/v")},
			"wildcard",
		},
		{
			"same name claimed twice",
			[]ListenerOption{
				AutoTLS("a.example").Email("x@x").Storage("/v"),
				StaticTLSFor("A.example", "c.pem", "k.pem"),
			},
			"both claim",
		},
		{
			"same wildcard claimed twice",
			[]ListenerOption{
				AutoTLS("*.a.example").Email("x@x").Storage("/v").CloudflareDNS01("tok"),
				StaticTLSFor("*.a.example", "c.pem", "k.pem"),
			},
			"both claim",
		},
		{
			"two hostless fallbacks",
			[]ListenerOption{
				StaticTLS("c1.pem", "k1.pem"),
				StaticTLS("c2.pem", "k2.pem"),
			},
			"one hostless fallback",
		},
		{
			"StaticTLSFor with blank host",
			[]ListenerOption{StaticTLSFor("  ", "c.pem", "k.pem")},
			"host required",
		},
		{
			"no TLS material at all",
			nil,
			"requires AutoTLS or StaticTLS",
		},
		{
			"two ACME sources claim one name",
			[]ListenerOption{
				AutoTLS("a.example").Email("x@x").Storage("/v"),
				AutoTLS("a.example").Email("x@x").Storage("/v"),
			},
			"both claim",
		},
		{
			"static source missing key file",
			[]ListenerOption{StaticTLS("c.pem", "")},
			"cert_file and key_file required",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := Resolve(tlsRouterConfig(c.opts...))
			if err == nil {
				t.Fatal("want resolve error")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %v missing %q", err, c.want)
			}
		})
	}
}

// mustAdd indexes one pattern in the router, failing the test if the name
// is already claimed.
func mustAdd(t *testing.T, cr *certRouter, pattern string, g certGetter) {
	t.Helper()
	if err := cr.add(pattern, g); err != nil {
		t.Fatalf("add %q: %v", pattern, err)
	}
}

// TestCertRouterSelection drives GetCertificate through a hand-built router
// whose getters record which source served the handshake.
func TestCertRouterSelection(t *testing.T) {
	t.Parallel()
	tag := func(name string, hit *string) certGetter {
		return func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			*hit = name
			return &tls.Certificate{}, nil
		}
	}
	var hit string
	cr := &certRouter{exact: map[string]certGetter{}, wildcards: map[string]certGetter{}}
	mustAdd(t, cr, "foo.bar.example", tag("exact", &hit))
	mustAdd(t, cr, "*.bar.example", tag("wildcard", &hit))

	cases := []struct {
		name, sni, want string
	}{
		{"exact beats wildcard", "foo.bar.example", "exact"},
		{"exact is case-insensitive with trailing dot", "FOO.Bar.Example.", "exact"},
		{"wildcard covers one extra label", "other.bar.example", "wildcard"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hit = ""
			if _, err := cr.GetCertificate(&tls.ClientHelloInfo{ServerName: c.sni}); err != nil {
				t.Fatal(err)
			}
			if hit != c.want {
				t.Errorf("SNI %q served by %q, want %q", c.sni, hit, c.want)
			}
		})
	}

	t.Run("fallback catches unmatched and SNI-less clients", func(t *testing.T) {
		cr.fallback = tag("fallback", &hit)
		defer func() { cr.fallback = nil }()
		for _, sni := range []string{"nope.example", ""} {
			hit = ""
			if _, err := cr.GetCertificate(&tls.ClientHelloInfo{ServerName: sni}); err != nil {
				t.Fatal(err)
			}
			if hit != "fallback" {
				t.Errorf("SNI %q served by %q, want fallback", sni, hit)
			}
		}
	})
}

// TestCertRouterMisses — without a fallback, a name no source covers must
// fail the handshake rather than serve an arbitrary certificate.
func TestCertRouterMisses(t *testing.T) {
	t.Parallel()
	cr := &certRouter{exact: map[string]certGetter{}, wildcards: map[string]certGetter{}}
	mustAdd(t, cr, "*.bar.example", func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		return &tls.Certificate{}, nil
	})

	cases := []struct {
		name, sni, want string
	}{
		{"wildcard covers exactly one label", "deep.other.bar.example", "no TLS source covers"},
		{"unmatched SNI", "nope.example", "no TLS source covers"},
		{"empty SNI", "", "no SNI hostname"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := cr.GetCertificate(&tls.ClientHelloInfo{ServerName: c.sni})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("SNI %q: error %v, want %q", c.sni, err, c.want)
			}
		})
	}
}

// TestCertRouterTLSConfigALPN replaces the per-manager ALPN tables: the
// acme-tls/1 protocol appears exactly when an HTTP-01 ACME source must
// answer TLS-ALPN-01 challenges on a listener not behind Cloudflare.
func TestCertRouterTLSConfigALPN(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		acme      bool // router carries an HTTP-01 ACME source
		http2, cf bool
		want      []string
	}{
		{"static only, h1", false, false, false, []string{"http/1.1"}},
		{"static only, h2", false, true, false, []string{"h2", "http/1.1"}},
		{"acme h1 public", true, false, false, []string{"http/1.1", "acme-tls/1"}},
		{"acme h2 public", true, true, false, []string{"h2", "http/1.1", "acme-tls/1"}},
		{"acme h2 behind cloudflare", true, true, true, []string{"h2", "http/1.1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cr := &certRouter{hasACMETLS: c.acme}
			l := &resolved.Listener{EnableHTTP2: c.http2, BehindCloudflare: c.cf}
			cfg := certRouterTLSConfig(cr, l)
			if !slices.Equal(cfg.NextProtos, c.want) {
				t.Errorf("NextProtos: got %v, want %v", cfg.NextProtos, c.want)
			}
			if cfg.MinVersion != tls.VersionTLS12 {
				t.Errorf("MinVersion: got %x", cfg.MinVersion)
			}
		})
	}
}

// TestSNIHandshakeRouting proves end to end that one listener serves
// different certificates by SNI: real TLS handshakes against the listener's
// tls.Config must surface the certificate whose DNS names cover the name
// the client asked for.
func TestSNIHandshakeRouting(t *testing.T) {
	t.Parallel()
	aCert, aKey := writeSelfSignedCert(t, "a.example")
	bCert, bKey := writeSelfSignedCert(t, "*.b.example")
	fbCert, fbKey := writeSelfSignedCert(t, "fallback.invalid")

	r := mustResolve(t, tlsRouterConfig(
		StaticTLSFor("a.example", aCert, aKey),
		StaticTLSFor("*.b.example", bCert, bKey),
		StaticTLS(fbCert, fbKey),
	))
	srv, err := newServer(r)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	cfg := certRouterTLSConfig(srv.certRouters[":443"], r.Listeners[0])

	cases := []struct {
		sni, wantDNS string
	}{
		{"a.example", "a.example"},
		{"x.b.example", "*.b.example"},
		{"unrelated.example", "fallback.invalid"},
		{"", "fallback.invalid"},
	}
	for _, c := range cases {
		name := c.sni
		if name == "" {
			name = "(no SNI)"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			state := handshake(t, cfg, c.sni)
			leaf := state.PeerCertificates[0]
			if !slices.Contains(leaf.DNSNames, c.wantDNS) {
				t.Errorf("SNI %q got certificate for %v, want %q", c.sni, leaf.DNSNames, c.wantDNS)
			}
		})
	}
}

// handshake completes one TLS handshake against serverCfg over an in-memory
// pipe and returns the client's view of the connection.
func handshake(t *testing.T, serverCfg *tls.Config, sni string) tls.ConnectionState {
	t.Helper()
	cp, sp := net.Pipe()
	srvErr := make(chan error, 1)
	go func() {
		conn := tls.Server(sp, serverCfg)
		srvErr <- conn.Handshake()
	}()
	client := tls.Client(cp, &tls.Config{
		ServerName:         sni,
		InsecureSkipVerify: true, //nolint:gosec // self-signed test certificates; routing is under test, not trust
		MinVersion:         tls.VersionTLS12,
	})
	if err := client.Handshake(); err != nil {
		t.Fatalf("client handshake (SNI %q): %v", sni, err)
	}
	if err := <-srvErr; err != nil {
		t.Fatalf("server handshake (SNI %q): %v", sni, err)
	}
	state := client.ConnectionState()
	_ = client.Close()
	return state
}

// TestNewServerStaticSourceLoadsEagerly — a bad static path fails server
// construction, not the first handshake.
func TestNewServerStaticSourceLoadsEagerly(t *testing.T) {
	t.Parallel()
	r := mustResolve(t, tlsRouterConfig(StaticTLSFor("a.example", "/nonexistent/c.pem", "/nonexistent/k.pem")))
	_, err := newServer(r)
	if err == nil || !strings.Contains(err.Error(), "static_tls: load") {
		t.Errorf("error: %v", err)
	}
}

// TestCanonicalTLSName — one canonicalisation for configured names and
// ClientHello lookups: case, one trailing dot, IDNA A-labels, and wildcard
// suffixes, with a lowercase fallback for names the IDNA lookup profile
// rejects.
func TestCanonicalTLSName(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"foo.example.com.", "foo.example.com"},
		{"FOO.EXAMPLE.COM", "foo.example.com"},
		{"münchen.example", "xn--mnchen-3ya.example"},
		{"*.example.com.", "*.example.com"},
		{"*.MÜNCHEN.example", "*.xn--mnchen-3ya.example"},
		{"foo_bar.internal", "foo_bar.internal"}, // IDNA-invalid: lowercase fallback
		{"  x.example  ", "x.example"},
		{"", ""},
	}
	for _, c := range cases {
		if got := canonicalTLSName(c.in); got != c.want {
			t.Errorf("canonicalTLSName(%q): got %q, want %q", c.in, got, c.want)
		}
	}
}

// TestResolveCanonicalisesAutoTLSDomains — a trailing dot, mixed case, or
// Unicode label in an AutoTLS domain must not defeat routing or duplicate
// detection: the resolved domains carry the canonical form, and a name
// spelled two ways still counts as one claim.
func TestResolveCanonicalisesAutoTLSDomains(t *testing.T) {
	t.Parallel()
	r := mustResolve(t, tlsRouterConfig(
		AutoTLS("FOO.Example.COM.", "münchen.example").Email("x@x").Storage("/v"),
	))
	got := r.Listeners[0].AutoTLSSources[0].Domains
	want := []string{"foo.example.com", "xn--mnchen-3ya.example"}
	if !slices.Equal(got, want) {
		t.Errorf("domains: got %v, want %v", got, want)
	}

	_, err := Resolve(tlsRouterConfig(
		AutoTLS("foo.example.com.").Email("x@x").Storage("/v"),
		StaticTLSFor("foo.example.com", "c.pem", "k.pem"),
	))
	if err == nil || !strings.Contains(err.Error(), "both claim") {
		t.Errorf("trailing-dot spelling must still collide: %v", err)
	}

	_, err = Resolve(tlsRouterConfig(AutoTLS(".").Email("x@x").Storage("/v")))
	if err == nil || !strings.Contains(err.Error(), "invalid domain") {
		t.Errorf("dot-only domain: %v", err)
	}
}

// TestCertRouterCanonicalLookup — a source configured with a trailing dot
// or IDN U-label serves the ClientHello spelling of the same name.
func TestCertRouterCanonicalLookup(t *testing.T) {
	t.Parallel()
	var hit string
	tag := func(name string) certGetter {
		return func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			hit = name
			return &tls.Certificate{}, nil
		}
	}
	cr := &certRouter{exact: map[string]certGetter{}, wildcards: map[string]certGetter{}}
	mustAdd(t, cr, "foo.example.com.", tag("dotted"))
	mustAdd(t, cr, "münchen.example", tag("idn"))

	cases := []struct{ sni, want string }{
		{"foo.example.com", "dotted"},
		{"xn--mnchen-3ya.example", "idn"},
	}
	for _, c := range cases {
		hit = ""
		if _, err := cr.GetCertificate(&tls.ClientHelloInfo{ServerName: c.sni}); err != nil {
			t.Fatalf("SNI %q: %v", c.sni, err)
		}
		if hit != c.want {
			t.Errorf("SNI %q served by %q, want %q", c.sni, hit, c.want)
		}
	}
}

// TestHTTP01PinIsEnforced — HTTP01() is a policy, not a comment: a pinned
// source never enters the shared autocert manager (whose challenge
// preference is hard-coded to attempt TLS-ALPN-01 first), gets its own
// HTTP-01-only acmeManager instead, and the listener does not advertise
// acme-tls/1 on its behalf.
func TestHTTP01PinIsEnforced(t *testing.T) {
	t.Parallel()
	r := mustResolve(t, withPlainHTTP(tlsRouterConfig(
		AutoTLS("foo.example.com").HTTP01().Email("x@x").Storage(t.TempDir()),
	)))
	srv, err := newServer(r)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	if srv.autocertMgr != nil {
		t.Error("pinned source must not feed the shared autocert manager")
	}
	src := r.Listeners[0].AutoTLSSources[0]
	m := srv.acmeManagers[src]
	if m == nil {
		t.Fatal("pinned source has no in-tree acme manager")
	}
	if _, ok := m.solver.(*http01Solver); !ok {
		t.Errorf("solver: got %T, want *http01Solver", m.solver)
	}
	if got := m.solver.challengeType(); got != "http-01" {
		t.Errorf("challenge type: got %q", got)
	}
	cr := srv.certRouters[":443"]
	if cr.hasACMETLS {
		t.Error("pinned HTTP-01 source must not mark the listener TLS-ALPN-capable")
	}
	if protos := certRouterTLSConfig(cr, r.Listeners[0]).NextProtos; slices.Contains(protos, "acme-tls/1") {
		t.Errorf("ALPN advertises acme-tls/1 for a pinned source: %v", protos)
	}
	if _, ok := cr.exact["foo.example.com"]; !ok {
		t.Errorf("router must index the pinned domain: %v", cr.exact)
	}
}

// TestAutoChallengeStaysTLSALPNCapable — without HTTP01() the automatic
// policy keeps advertising acme-tls/1, exactly as before.
func TestAutoChallengeStaysTLSALPNCapable(t *testing.T) {
	t.Parallel()
	r := mustResolve(t, tlsRouterConfig(
		AutoTLS("foo.example.com").Email("x@x").Storage("/v"),
	))
	if got := r.Listeners[0].AutoTLSSources[0].Challenge; got != resolved.ChallengeAuto {
		t.Errorf("default challenge: got %v, want ChallengeAuto", got)
	}
	srv, err := newServer(r)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	cr := srv.certRouters[":443"]
	if !cr.hasACMETLS {
		t.Error("automatic source must keep the listener TLS-ALPN-capable")
	}
	if protos := certRouterTLSConfig(cr, r.Listeners[0]).NextProtos; !slices.Contains(protos, "acme-tls/1") {
		t.Errorf("ALPN must advertise acme-tls/1 for the automatic policy: %v", protos)
	}
}

// TestHTTP01TokensServedOnPlainListener — the plain HTTP listener's handler
// chain consults every pinned HTTP-01 manager's token table, so challenges
// complete on :80 exactly like autocert's do.
func TestHTTP01TokensServedOnPlainListener(t *testing.T) {
	t.Parallel()
	cfg := tlsRouterConfig(AutoTLS("foo.example.com").HTTP01().Email("x@x").Storage(t.TempDir()))
	cfg.Listeners = append(Listeners{HTTP(":80")}, cfg.Listeners...)
	r := mustResolve(t, cfg)
	srv, err := newServer(r)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	src := r.Listeners[1].AutoTLSSources[0]
	solver, ok := srv.acmeManagers[src].solver.(*http01Solver)
	if !ok {
		t.Fatalf("solver: got %T", srv.acmeManagers[src].solver)
	}
	path := "/.well-known/acme-challenge/tok-9"
	solver.mu.Lock()
	solver.tokens[path] = "tok-9.keyauth"
	solver.mu.Unlock()

	h := srv.buildListenerHandler(r.Listeners[0], srv.buildRouter())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://foo.example.com"+path, nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "tok-9.keyauth" {
		t.Errorf("challenge on plain listener: code=%d body=%q", rec.Code, rec.Body.String())
	}
}

// TestBuildCertRouterGuards — the router refuses to index an ACME source
// whose runtime manager is missing; these states are internal invariant
// violations, so they must fail loudly instead of handing out nil getters.
func TestBuildCertRouterGuards(t *testing.T) {
	t.Parallel()
	s := &server{} // no managers initialised
	dns01 := &resolved.Listener{AutoTLSSources: []*resolved.AutoTLS{
		{
			Domains:   []string{"*.a.example"},
			DNS01:     &resolved.CloudflareDNS01{APIToken: "t"},
			Challenge: resolved.ChallengeDNS01,
		},
	}}
	if _, err := s.buildCertRouter(dns01); err == nil || !strings.Contains(err.Error(), "acme manager not initialised") {
		t.Errorf("dns01 source without manager: %v", err)
	}
	auto := &resolved.Listener{AutoTLSSources: []*resolved.AutoTLS{
		{Domains: []string{"a.example"}},
	}}
	if _, err := s.buildCertRouter(auto); err == nil || !strings.Contains(err.Error(), "manager not initialised") {
		t.Errorf("automatic source without manager: %v", err)
	}
	pinned := &resolved.Listener{AutoTLSSources: []*resolved.AutoTLS{
		{Domains: []string{"a.example"}, Challenge: resolved.ChallengeHTTP01},
	}}
	if _, err := s.buildCertRouter(pinned); err == nil || !strings.Contains(err.Error(), "manager not initialised") {
		t.Errorf("pinned source without manager: %v", err)
	}
}

// TestHTTP01SolverServesTokens — the pinned manager's token table answers
// exactly the pending challenge paths on the plain HTTP listener and passes
// everything else through.
func TestHTTP01SolverServesTokens(t *testing.T) {
	t.Parallel()
	solver := newHTTP01Solver()
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	h := solver.wrap(next)

	path := "/.well-known/acme-challenge/tok-1"
	solver.mu.Lock()
	solver.tokens[path] = "tok-1.keyauth"
	solver.mu.Unlock()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "tok-1.keyauth" {
		t.Errorf("challenge path: code=%d body=%q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/other", nil))
	if rec.Code != http.StatusTeapot {
		t.Errorf("pass-through: code=%d, want 418", rec.Code)
	}

	solver.mu.Lock()
	delete(solver.tokens, path)
	solver.mu.Unlock()
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	if rec.Code != http.StatusTeapot {
		t.Errorf("expired token must fall through: code=%d, want 418", rec.Code)
	}
}

// TestBuildHTTP3ServerRequiresRouter — HTTP/3 rides the parent HTTPS
// listener's cert router; a listener that never built one cannot serve QUIC.
func TestBuildHTTP3ServerRequiresRouter(t *testing.T) {
	t.Parallel()
	s := &server{}
	_, err := s.buildHTTP3Server(&resolved.Listener{Addr: ":443", HTTP3Addr: ":443/udp"}, nil)
	if err == nil || !strings.Contains(err.Error(), "requires AutoTLS or StaticTLS") {
		t.Errorf("error: %v", err)
	}
}

// TestNewServerDNS01BadStorage — a DNS-01 storage path that cannot become a
// directory fails server construction with the listener named.
func TestNewServerDNS01BadStorage(t *testing.T) {
	t.Parallel()
	f := t.TempDir() + "/occupied"
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := mustResolve(t, tlsRouterConfig(
		AutoTLS("*.bar.example").Email("x@x").Storage(f).CloudflareDNS01("tok"),
	))
	_, err := newServer(r)
	if err == nil || !strings.Contains(err.Error(), "acme manager") {
		t.Errorf("error: %v", err)
	}
}

// TestServeListenerTLS — an HTTPS content listener serves through the cert
// router on hs.TLSConfig; ServeTLS receives no file paths.
func TestServeListenerTLS(t *testing.T) {
	t.Parallel()
	certFile, keyFile := writeSelfSignedCert(t, "x.example")
	r := mustResolve(t, tlsRouterConfig(StaticTLS(certFile, keyFile)))
	srv, err := newServer(r)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	l := r.Listeners[0]
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }),
		TLSConfig:         certRouterTLSConfig(srv.certRouters[":443"], l),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go serveListener(hs, l, ln)
	defer func() { _ = hs.Close() }()

	conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{
		ServerName:         "x.example",
		InsecureSkipVerify: true, //nolint:gosec // self-signed test certificate
		MinVersion:         tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	leaf := conn.ConnectionState().PeerCertificates[0]
	if !slices.Contains(leaf.DNSNames, "x.example") {
		t.Errorf("served certificate for %v", leaf.DNSNames)
	}
}

// TestNewServerDNS01SourceGetsOwnManager — each DNS-01 source is backed by
// its own manager, and the listener's router indexes its wildcard pattern.
func TestNewServerDNS01SourceGetsOwnManager(t *testing.T) {
	t.Parallel()
	fbCert, fbKey := writeSelfSignedCert(t, "fallback.invalid")
	r := mustResolve(t, tlsRouterConfig(
		AutoTLS("*.bar.example").Email("x@x").Storage(t.TempDir()).CloudflareDNS01("tok"),
		StaticTLS(fbCert, fbKey),
	))
	srv, err := newServer(r)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	if n := len(srv.acmeManagers); n != 1 {
		t.Fatalf("acme managers: got %d, want 1", n)
	}
	cr := srv.certRouters[":443"]
	if cr == nil {
		t.Fatal("no cert router stored for :443")
	}
	if _, ok := cr.wildcards["*.bar.example"]; !ok {
		t.Errorf("router wildcards missing DNS-01 pattern: %v", cr.wildcards)
	}
	if cr.hasACMETLS {
		t.Error("DNS-01-only ACME must not advertise acme-tls/1")
	}
	if cr.fallback == nil {
		t.Error("hostless static source must become the fallback")
	}
}

// TestCanonicalTLSNameIdempotent — canonicalisation must be a fixed point.
// Resolve compares canonical names for duplicate coverage and
// certRouter.add canonicalises again when it indexes the source, so a name
// whose second pass differs from its first would pass validation as
// distinct from its own other spelling and then collide in the router's
// map, dropping one configured certificate.
func TestCanonicalTLSNameIdempotent(t *testing.T) {
	t.Parallel()
	nasty := []string{
		"example.com..",
		"example.com .",
		"  EXAMPLE.com . . ",
		"*.example.com..",
		"*.MÜNCHEN.example. .",
		"foo_bar.internal..",
		"*.",
		"*..",
		"*",
		".",
		"..",
		"",
	}
	for _, in := range nasty {
		once := canonicalTLSName(in)
		if twice := canonicalTLSName(once); twice != once {
			t.Errorf("canonicalTLSName(%q) = %q, not a fixed point: %q", in, once, twice)
		}
	}
	fixed := []struct{ in, want string }{
		{"example.com..", "example.com"},
		{"example.com .", "example.com"},
		{"*.example.com..", "*.example.com"},
		{"*.", "*"},
		{"..", ""},
	}
	for _, c := range fixed {
		if got := canonicalTLSName(c.in); got != c.want {
			t.Errorf("canonicalTLSName(%q): got %q, want %q", c.in, got, c.want)
		}
	}
}

// TestResolveRejectsRepeatedTrailingDots — "example.com.." and
// "example.com" are one name for routing, so declaring both must collide
// at resolve time rather than reaching the router, where the second source
// would silently take the first source's map entry.
func TestResolveRejectsRepeatedTrailingDots(t *testing.T) {
	t.Parallel()
	_, err := Resolve(tlsRouterConfig(
		AutoTLS("example.com..").Email("x@x").Storage("/v"),
		StaticTLSFor("example.com", "c.pem", "k.pem"),
	))
	if err == nil || !strings.Contains(err.Error(), "both claim") {
		t.Errorf("repeated trailing dots must still collide: %v", err)
	}
}

// TestCertRouterAddRejectsDuplicate — add refuses a name already indexed
// instead of overwriting it. Resolve-time coverage validation should make
// this unreachable; losing a configured certificate to map-write order
// must be structurally impossible even if it ever slips.
func TestCertRouterAddRejectsDuplicate(t *testing.T) {
	t.Parallel()
	cr := &certRouter{exact: map[string]certGetter{}, wildcards: map[string]certGetter{}}
	g := func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &tls.Certificate{}, nil }
	if err := cr.add("example.com..", g); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if err := cr.add("EXAMPLE.com.", g); err == nil || !strings.Contains(err.Error(), "claimed by two sources") {
		t.Errorf("duplicate exact name: %v", err)
	}
	if err := cr.add("*.example.com", g); err != nil {
		t.Fatalf("first wildcard add: %v", err)
	}
	if err := cr.add("*.EXAMPLE.com..", g); err == nil || !strings.Contains(err.Error(), "claimed by two sources") {
		t.Errorf("duplicate wildcard pattern: %v", err)
	}
}

// TestBuildCertRouterPropagatesDuplicate — the duplicate is an error out
// of buildCertRouter, not a dropped source inside it.
func TestBuildCertRouterPropagatesDuplicate(t *testing.T) {
	t.Parallel()
	s := &server{autocertMgr: &autocert.Manager{}}
	l := &resolved.Listener{AutoTLSSources: []*resolved.AutoTLS{
		{Domains: []string{"a.example"}},
		{Domains: []string{"A.example."}},
	}}
	if _, err := s.buildCertRouter(l); err == nil || !strings.Contains(err.Error(), "claimed by two sources") {
		t.Errorf("buildCertRouter must propagate the duplicate: %v", err)
	}
}

// TestResolveStaticTLSKeepsLenientNames — the lowercase fallback for
// IDNA-rejected names stays available to static hosts: an underscore
// hostname is unusual but routes fine when the operator supplies the
// certificate themselves.
func TestResolveStaticTLSKeepsLenientNames(t *testing.T) {
	t.Parallel()
	r := mustResolve(t, tlsRouterConfig(StaticTLSFor("MY_APP.example.com.", "c.pem", "k.pem")))
	if got := r.Listeners[0].StaticTLSSources[0].Host; got != "my_app.example.com" {
		t.Errorf("static host: got %q, want %q", got, "my_app.example.com")
	}
}

// TestResolveTLSNameShapeErrors — names that canonicalise to something no
// handshake can select, or that no CA can issue, are resolve errors rather
// than silently dead configuration.
func TestResolveTLSNameShapeErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		opts []ListenerOption
		want string
	}{
		{
			"bare star static host",
			[]ListenerOption{StaticTLSFor("*", "c.pem", "k.pem")},
			"use StaticTLS for the hostless fallback",
		},
		{
			"star dot static host",
			[]ListenerOption{StaticTLSFor("*.", "c.pem", "k.pem")},
			`"*" matches no SNI name`,
		},
		{
			"nested wildcard static host",
			[]ListenerOption{StaticTLSFor("*.*.example.com", "c.pem", "k.pem")},
			`single leading "*." label`,
		},
		{
			"bare star ACME domain",
			[]ListenerOption{AutoTLS("*").Email("x@x").Storage("/v").CloudflareDNS01("tok")},
			`"*" matches no SNI name`,
		},
		{
			"nested wildcard ACME domain",
			[]ListenerOption{AutoTLS("*.*.example.com").Email("x@x").Storage("/v").CloudflareDNS01("tok")},
			`single leading "*." label`,
		},
		{
			"underscore ACME domain",
			[]ListenerOption{AutoTLS("my_app.example.com").Email("x@x").Storage("/v")},
			"not a valid ACME identifier",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := Resolve(tlsRouterConfig(c.opts...))
			if err == nil {
				t.Fatal("want resolve error")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %v missing %q", err, c.want)
			}
		})
	}
}

// multiListenerConfig wraps the given listeners with the pool and route the
// resolver requires, for the checks that only a whole config can fail.
func multiListenerConfig(ls ...*Listener) Config {
	return Config{
		Listeners: ls,
		Upstreams: Upstreams{"a": Pool{Backends: []Backend{{Address: "127.0.0.1:1"}}}},
		Routes:    Routes{Match("/*").ProxyTo("a")},
	}
}

// TestHTTP01RequiresPlainListener — the pinned manager's token table is
// layered onto plain HTTP listeners only, so an HTTPS-only config can
// never complete an HTTP-01 challenge; it must fail at resolve instead of
// burning a failed validation per start. The bind address is not checked:
// RFC 8555 §8.3 fixes the validator to port 80, but operators port-map.
func TestHTTP01RequiresPlainListener(t *testing.T) {
	t.Parallel()
	pinned := func() ListenerOption {
		return AutoTLS("foo.example.com").HTTP01().Email("x@x").Storage("/v")
	}
	_, err := Resolve(multiListenerConfig(HTTPS(":443", pinned())))
	if err == nil {
		t.Fatal("HTTP-01 source without a plain HTTP listener must not resolve")
	}
	if !strings.Contains(err.Error(), "requires a plain HTTP listener") {
		t.Errorf("error %v missing the reason", err)
	}
	if !strings.Contains(err.Error(), "foo.example.com") {
		t.Errorf("error must name the source's domains: %v", err)
	}

	mustResolve(t, multiListenerConfig(
		HTTP(":8080"), // port-mapped to 80 upstream; the address is not checked
		HTTPS(":443", pinned()),
	))
	mustResolve(t, multiListenerConfig(
		HTTP(":80").RedirectTo("https"), // challenges are served ahead of the redirect
		HTTPS(":443", pinned()),
	))
	mustResolve(t, multiListenerConfig(
		HTTPS(":443", AutoTLS("foo.example.com").Email("x@x").Storage("/v")),
	)) // the automatic policy may still reach HTTP-01, but never only that
}

// TestPinnedDomainCollisions — one in-tree manager is built per pinned
// source and persists to <storage>/<challenge>/<domain>.{crt,key}, so two
// pinned sources sharing that path race to rename over each other's key
// pair and must not resolve. Everything without a shared file stays
// legal: automatic sources feed one shared autocert manager whose domain
// list is a union (the pre-PR two-listener shape keeps working), distinct
// storage roots and distinct challenge kinds never collide, and static
// hosts share no issuance state at all.
func TestPinnedDomainCollisions(t *testing.T) {
	t.Parallel()
	_, err := Resolve(multiListenerConfig(
		HTTPS(":443", AutoTLS("dup.example").Email("x@x").Storage("/v").CloudflareDNS01("tok")),
		HTTPS(":8443", AutoTLS("DUP.example.").Email("x@x").Storage("/v").CloudflareDNS01("tok")),
	))
	if err == nil || !strings.Contains(err.Error(), "same path") {
		t.Errorf("pinned sources sharing one cert path: %v", err)
	}

	// The exact shape that works on master: one domain on two automatic
	// listeners, unioned into the shared autocert manager.
	mustResolve(t, multiListenerConfig(
		HTTPS(":443", AutoTLS("dup.example").Email("x@x").Storage("/v")),
		HTTPS(":8443", AutoTLS("dup.example").Email("x@x").Storage("/v")),
	))

	// Pinned, but distinct storage roots: two managers, two files.
	mustResolve(t, multiListenerConfig(
		HTTPS(":443", AutoTLS("dup.example").Email("x@x").Storage("/v1").CloudflareDNS01("tok")),
		HTTPS(":8443", AutoTLS("dup.example").Email("x@x").Storage("/v2").CloudflareDNS01("tok")),
	))

	// Pinned, same root, different challenge kinds: dns01/ vs http01/.
	cfg := multiListenerConfig(
		HTTP(":80"),
		HTTPS(":443", AutoTLS("dup.example").Email("x@x").Storage("/v").CloudflareDNS01("tok")),
		HTTPS(":8443", AutoTLS("dup.example").Email("x@x").Storage("/v").HTTP01()),
	)
	mustResolve(t, cfg)

	mustResolve(t, multiListenerConfig(
		HTTPS(":443", StaticTLSFor("dup.example", "c.pem", "k.pem")),
		HTTPS(":8443", StaticTLSFor("dup.example", "c.pem", "k.pem")),
	))
}

// TestPinnedACMEAccountEmailMismatch — pinned sources sharing
// <storage>/<challenge>/account.key share one ACME account, and
// Client.Register returns ErrAccountAlreadyExists for the second without
// applying its contact, so one email would be lost by map iteration order.
// Different challenge subdirectories are different accounts and may differ.
func TestPinnedACMEAccountEmailMismatch(t *testing.T) {
	t.Parallel()
	_, err := Resolve(multiListenerConfig(
		HTTPS(":443", AutoTLS("a.example").Email("a@example.com").Storage("/v").CloudflareDNS01("tok")),
		HTTPS(":8443", AutoTLS("b.example").Email("b@example.com").Storage("/v").CloudflareDNS01("tok")),
	))
	if err == nil || !strings.Contains(err.Error(), "email mismatch") {
		t.Errorf("divergent email on one ACME account: %v", err)
	}

	mustResolve(t, multiListenerConfig(
		HTTP(":80"),
		HTTPS(":443", AutoTLS("a.example").Email("a@example.com").Storage("/v").CloudflareDNS01("tok")),
		HTTPS(":8443", AutoTLS("b.example").Email("b@example.com").Storage("/v").HTTP01()),
	))
}
