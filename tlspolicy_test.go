package statute

import (
	"crypto/tls"
	"net"
	"slices"
	"strings"
	"testing"

	"statute.kjanat.dev/resolved"
)

// TestResolveTLSPolicy — the issue #38 shape resolves into the normalised
// schema: versions as "1.2"/"1.3", suites as IANA names in declaration
// order.
func TestResolveTLSPolicy(t *testing.T) {
	t.Parallel()
	r := mustResolve(t, tlsRouterConfig(
		AutoTLS("foo.example.test").Email("ops@example.test").Storage("/var/lib/statute/acme"),
		TLSPolicy{
			MinVersion: TLS12,
			MaxVersion: TLS13,
			CipherSuites: []CipherSuite{
				TLSECDHEECDSAWithAES128GCM,
				TLSECDHERSAWithAES128GCM,
			},
		},
	))
	p := r.Listeners[0].TLSPolicy
	if p == nil {
		t.Fatal("resolved listener carries no TLS policy")
	}
	if p.MinVersion != "1.2" || p.MaxVersion != "1.3" {
		t.Errorf("versions: got %q/%q, want \"1.2\"/\"1.3\"", p.MinVersion, p.MaxVersion)
	}
	want := []string{
		"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256",
		"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
	}
	if !slices.Equal(p.CipherSuites, want) {
		t.Errorf("cipher suites: got %v, want %v", p.CipherSuites, want)
	}
}

// TestResolveTLSPolicyPartial — an unset bound normalises to the empty
// string rather than a fabricated default, and a listener that declares no
// policy at all leaves the field nil.
func TestResolveTLSPolicyPartial(t *testing.T) {
	t.Parallel()
	r := mustResolve(t, tlsRouterConfig(
		StaticTLS("cert.pem", "key.pem"),
		TLSPolicy{MinVersion: TLS13},
	))
	p := r.Listeners[0].TLSPolicy
	if p == nil {
		t.Fatal("resolved listener carries no TLS policy")
	}
	if p.MinVersion != "1.3" || p.MaxVersion != "" || p.CipherSuites != nil {
		t.Errorf("policy: got %+v, want min 1.3 and the rest unset", *p)
	}

	none := mustResolve(t, tlsRouterConfig(StaticTLS("cert.pem", "key.pem")))
	if got := none.Listeners[0].TLSPolicy; got != nil {
		t.Errorf("listener without a policy: got %+v, want nil", got)
	}
}

// TestResolveTLSPolicyErrors — every rejection the policy makes, each with
// the shape that reaches it.
func TestResolveTLSPolicyErrors(t *testing.T) {
	t.Parallel()
	// A policy can only reach a plain HTTP listener through the option
	// interface; HTTP takes no options, so build that one by hand.
	plainWithPolicy := func() Config {
		l := HTTP(":80")
		TLSPolicy{MinVersion: TLS12}.applyListener(l)
		return multiListenerConfig(l)
	}

	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			"policy on a non-HTTPS listener",
			plainWithPolicy(),
			"only an HTTPS listener",
		},
		{
			"policy declared twice",
			tlsRouterConfig(
				StaticTLS("cert.pem", "key.pem"),
				TLSPolicy{MinVersion: TLS12},
				TLSPolicy{MaxVersion: TLS13},
			),
			"declared 2 times",
		},
		{
			"unsupported min version",
			tlsRouterConfig(
				StaticTLS("cert.pem", "key.pem"),
				TLSPolicy{MinVersion: TLSVersion(tls.VersionTLS10)},
			),
			"min_version 0x0301 is not a supported version",
		},
		{
			"unsupported max version",
			tlsRouterConfig(
				StaticTLS("cert.pem", "key.pem"),
				TLSPolicy{MaxVersion: TLSVersion(tls.VersionTLS11)},
			),
			"max_version 0x0302 is not a supported version",
		},
		{
			"inverted version window",
			tlsRouterConfig(
				StaticTLS("cert.pem", "key.pem"),
				TLSPolicy{MinVersion: TLS13, MaxVersion: TLS12},
			),
			"min_version 1.3 is above max_version 1.2",
		},
		{
			"unknown cipher suite",
			tlsRouterConfig(
				StaticTLS("cert.pem", "key.pem"),
				TLSPolicy{CipherSuites: []CipherSuite{CipherSuite(tls.TLS_RSA_WITH_AES_128_GCM_SHA256)}},
			),
			"is not one of the suites statute exposes",
		},
		{
			"cipher suite listed twice",
			tlsRouterConfig(
				StaticTLS("cert.pem", "key.pem"),
				TLSPolicy{CipherSuites: []CipherSuite{
					TLSECDHEECDSAWithAES128GCM,
					TLSECDHEECDSAWithAES128GCM,
				}},
			),
			"is listed twice",
		},
		{
			"cipher suites under a TLS 1.3 floor",
			tlsRouterConfig(
				StaticTLS("cert.pem", "key.pem"),
				TLSPolicy{
					MinVersion:   TLS13,
					CipherSuites: []CipherSuite{TLSECDHEECDSAWithAES128GCM},
				},
			),
			"governs TLS 1.2 handshakes only",
		},
		{
			// No HTTP2() and no version cap: the net/http check runs on
			// every ServeTLS regardless, so the rule must fire here too.
			"cipher suites omitting both AES-128-GCM suites",
			tlsRouterConfig(
				StaticTLS("cert.pem", "key.pem"),
				TLSPolicy{CipherSuites: []CipherSuite{
					TLSECDHEECDSAWithAES256GCM,
					TLSECDHEECDSAWithChaCha20,
				}},
			),
			"must include statute.TLSECDHEECDSAWithAES128GCM",
		},
		{
			// The static fallback cannot rescue the pinned source's
			// domain: the router picks the matching source and never
			// falls back past it, so this must fail despite the RSA
			// fallback certificate plausibly serving the policy fine.
			"RSA-only suites cannot serve a pinned source's domains",
			withPlainHTTP(tlsRouterConfig(
				AutoTLS("foo.example.test").HTTP01().Email("ops@example.test").Storage("/var/lib/statute/acme"),
				StaticTLS("cert.pem", "key.pem"),
				TLSPolicy{
					MaxVersion:   TLS12,
					CipherSuites: []CipherSuite{TLSECDHERSAWithAES128GCM},
				},
			)),
			"never falls back past the source",
		},
		{
			"policy on a redirect-only listener",
			multiListenerConfig(HTTPS(":443", TLSPolicy{MinVersion: TLS12}).RedirectTo("http")),
			"redirect-only listener terminates no TLS",
		},
		{
			"HTTP/3 under a 1.2 cap",
			tlsRouterConfig(
				StaticTLS("cert.pem", "key.pem"),
				HTTP3(":443/udp"),
				TLSPolicy{MaxVersion: TLS12},
			),
			"QUIC requires TLS 1.3",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := Resolve(c.cfg)
			if err == nil {
				t.Fatalf("want error containing %q, got nil", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not contain %q", err, c.want)
			}
			if !strings.Contains(err.Error(), "tls_policy: ") {
				t.Errorf("error %q is not prefixed tls_policy:", err)
			}
		})
	}
}

// TestResolveTLSPolicyLegalShapes — configurations adjacent to the suite
// rejections that must stay legal: CBC suites riding alongside the
// required AES-128-GCM one (h2 on and capped at 1.2 — h2 never negotiates
// the CBC entry, but TLS 1.2 HTTP/1.1 clients can), an RSA-only list
// rescued by TLS 1.3 staying available, an RSA-only capped list on
// automatic sources (autocert issues RSA to RSA-only clients — lint rule
// TLS004's territory, not resolve's), and one on static sources, whose
// key type resolve cannot know.
func TestResolveTLSPolicyLegalShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  Config
	}{
		{
			"CBC alongside the required AES-128-GCM suite",
			tlsRouterConfig(
				StaticTLS("cert.pem", "key.pem"),
				HTTP2(),
				TLSPolicy{
					MaxVersion: TLS12,
					CipherSuites: []CipherSuite{
						TLSECDHEECDSAWithAES128CBC,
						TLSECDHEECDSAWithAES128GCM,
					},
				},
			),
		},
		{
			"RSA-only list with TLS 1.3 still available",
			tlsRouterConfig(
				AutoTLS("foo.example.test").Email("ops@example.test").Storage("/var/lib/statute/acme"),
				TLSPolicy{CipherSuites: []CipherSuite{TLSECDHERSAWithAES128GCM}},
			),
		},
		{
			"RSA-only capped list on automatic sources",
			tlsRouterConfig(
				AutoTLS("foo.example.test").Email("ops@example.test").Storage("/var/lib/statute/acme"),
				StaticTLS("cert.pem", "key.pem"),
				TLSPolicy{
					MaxVersion:   TLS12,
					CipherSuites: []CipherSuite{TLSECDHERSAWithAES128GCM},
				},
			),
		},
		{
			"RSA-only capped list on static sources",
			tlsRouterConfig(
				StaticTLS("cert.pem", "key.pem"),
				TLSPolicy{
					MaxVersion:   TLS12,
					CipherSuites: []CipherSuite{TLSECDHERSAWithAES128GCM},
				},
			),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			mustResolve(t, c.cfg)
		})
	}
}

// TestCertRouterTLSConfigPolicy — the resolved policy lands on the
// listener's tls.Config, and a listener without one keeps the defaults the
// runtime has always used.
func TestCertRouterTLSConfigPolicy(t *testing.T) {
	t.Parallel()
	l := &resolved.Listener{TLSPolicy: &resolved.TLSPolicy{
		MinVersion: "1.2",
		MaxVersion: "1.2",
		CipherSuites: []string{
			"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256",
			"TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384",
		},
	}}
	cfg := certRouterTLSConfig(&certRouter{}, l)
	if cfg.MinVersion != tls.VersionTLS12 || cfg.MaxVersion != tls.VersionTLS12 {
		t.Errorf("versions: got %#04x/%#04x", cfg.MinVersion, cfg.MaxVersion)
	}
	want := []uint16{
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	}
	if !slices.Equal(cfg.CipherSuites, want) {
		t.Errorf("cipher suites: got %v, want %v", cfg.CipherSuites, want)
	}

	base := certRouterTLSConfig(&certRouter{}, &resolved.Listener{})
	if base.MinVersion != tls.VersionTLS12 || base.MaxVersion != 0 || base.CipherSuites != nil {
		t.Errorf("no policy: got min %#04x, max %#04x, suites %v; want the untouched defaults", base.MinVersion, base.MaxVersion, base.CipherSuites)
	}
}

// TestHTTP3ServerCarriesTLSPolicy — QUIC shares the cert router, so it
// inherits the same policy; only the ALPN list is HTTP/3's own.
func TestHTTP3ServerCarriesTLSPolicy(t *testing.T) {
	t.Parallel()
	certFile, keyFile := writeSelfSignedCert(t, "x.example")
	r := mustResolve(t, tlsRouterConfig(
		StaticTLS(certFile, keyFile),
		HTTP2(),
		HTTP3(":443/udp"),
		TLSPolicy{MinVersion: TLS13},
	))
	srv, err := newServer(r)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	h3, err := srv.buildHTTP3Server(r.Listeners[0], nil)
	if err != nil {
		t.Fatalf("buildHTTP3Server: %v", err)
	}
	cfg := h3.srv.TLSConfig
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion: got %#04x, want %#04x", cfg.MinVersion, tls.VersionTLS13)
	}
	if !slices.Equal(cfg.NextProtos, []string{alpnHTTP3}) {
		t.Errorf("NextProtos: got %v, want [%s]", cfg.NextProtos, alpnHTTP3)
	}
}

// TestTLSPolicyHandshake — real handshakes over net.Pipe prove the policy
// reaches the wire: a 1.2 cap ends in a TLS 1.2 connection, and a
// single-suite list is the suite that gets negotiated. The test
// certificates are ECDSA P-256, so the ECDSA suites are the usable ones.
func TestTLSPolicyHandshake(t *testing.T) {
	t.Parallel()
	certFile, keyFile := writeSelfSignedCert(t, "x.example")

	t.Run("max version caps the connection", func(t *testing.T) {
		t.Parallel()
		cfg := policyServerConfig(t, certFile, keyFile, TLSPolicy{MaxVersion: TLS12})
		if got := handshake(t, cfg, "x.example").Version; got != tls.VersionTLS12 {
			t.Errorf("negotiated version %#04x, want %#04x", got, tls.VersionTLS12)
		}
	})

	t.Run("cipher suites pin the negotiation", func(t *testing.T) {
		t.Parallel()
		cfg := policyServerConfig(t, certFile, keyFile, TLSPolicy{
			MaxVersion:   TLS12,
			CipherSuites: []CipherSuite{TLSECDHEECDSAWithAES128GCM},
		})
		state := handshake(t, cfg, "x.example")
		if got := state.CipherSuite; got != uint16(TLSECDHEECDSAWithAES128GCM) {
			t.Errorf("negotiated suite %s, want %s", tls.CipherSuiteName(got), tls.CipherSuiteName(uint16(TLSECDHEECDSAWithAES128GCM)))
		}
		// The discriminating half: Go's own TLS 1.2 preference would pick
		// AES-128-GCM anyway, so the success above cannot distinguish the
		// pin from the defaults. A client that only speaks a suite the
		// policy excludes can — the default server suite set includes
		// AES-256-GCM, so only the pin explains a refusal here.
		if err := handshakeErr(cfg, &tls.Config{
			ServerName:         "x.example",
			InsecureSkipVerify: true, //nolint:gosec // self-signed test certificate; the suite pin is under test, not trust
			MinVersion:         tls.VersionTLS12,
			MaxVersion:         tls.VersionTLS12,
			CipherSuites:       []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384},
		}); err == nil {
			t.Error("client restricted to an excluded suite completed a handshake; the pin is not reaching the wire")
		}
	})
}

// handshakeErr runs one net.Pipe handshake with the caller's client config
// and returns the client-side error, letting a test assert a refusal where
// handshake would t.Fatal.
func handshakeErr(serverCfg, clientCfg *tls.Config) error {
	cp, sp := net.Pipe()
	go func() {
		conn := tls.Server(sp, serverCfg)
		_ = conn.Handshake()
		_ = sp.Close()
	}()
	client := tls.Client(cp, clientCfg)
	err := client.Handshake()
	// Close the raw pipe rather than sending a close_notify no one reads;
	// see the handshake helper.
	_ = cp.Close()
	return err
}

// policyServerConfig resolves a one-listener config carrying the policy and
// returns the listener's server-side tls.Config.
func policyServerConfig(t *testing.T, certFile, keyFile string, p TLSPolicy) *tls.Config {
	t.Helper()
	r := mustResolve(t, tlsRouterConfig(StaticTLS(certFile, keyFile), p))
	srv, err := newServer(r)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	return certRouterTLSConfig(srv.certRouters[":443"], r.Listeners[0])
}

// TestApplyTLSPolicyUnknownNamesFailClosed — the resolved schema is the
// only writer of these names, but a hand-built one must not widen the
// suite list or move a version bound by accident: unknown versions are
// ignored, and a suite list where nothing maps becomes the empty non-nil
// list crypto/tls reads as "no TLS 1.2 suites at all".
func TestApplyTLSPolicyUnknownNamesFailClosed(t *testing.T) {
	t.Parallel()
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	applyTLSPolicy(cfg, &resolved.TLSPolicy{
		MinVersion:   "1.1",
		MaxVersion:   "",
		CipherSuites: []string{"TLS_RSA_WITH_AES_128_GCM_SHA256"},
	})
	if cfg.MinVersion != tls.VersionTLS12 || cfg.MaxVersion != 0 {
		t.Errorf("got min %#04x, max %#04x; want the bounds untouched", cfg.MinVersion, cfg.MaxVersion)
	}
	if cfg.CipherSuites == nil || len(cfg.CipherSuites) != 0 {
		t.Errorf("suites: got %v, want the empty non-nil fail-closed list", cfg.CipherSuites)
	}
	cfg.CipherSuites = nil
	applyTLSPolicy(cfg, nil)
	if cfg.MinVersion != tls.VersionTLS12 || cfg.CipherSuites != nil {
		t.Errorf("nil policy changed the config: min %#04x, suites %v", cfg.MinVersion, cfg.CipherSuites)
	}
}

// TestCipherSuiteNames — the IANA names the resolved schema promises,
// written out literally so the assertion is independent of the table that
// produces them, plus the backward mapping applyTLSPolicy relies on.
func TestCipherSuiteNames(t *testing.T) {
	t.Parallel()
	want := map[CipherSuite]string{
		TLSECDHEECDSAWithAES128GCM: "TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256",
		TLSECDHERSAWithAES128GCM:   "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
		TLSECDHEECDSAWithAES256GCM: "TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384",
		TLSECDHERSAWithAES256GCM:   "TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384",
		TLSECDHEECDSAWithChaCha20:  "TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256",
		TLSECDHERSAWithChaCha20:    "TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256",
		TLSECDHEECDSAWithAES128CBC: "TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA",
		TLSECDHERSAWithAES128CBC:   "TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA",
		TLSECDHEECDSAWithAES256CBC: "TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA",
		TLSECDHERSAWithAES256CBC:   "TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA",
	}
	if len(tls12CipherSuites) != len(want) {
		t.Fatalf("exposed suites: got %d, want %d", len(tls12CipherSuites), len(want))
	}
	for _, cs := range tls12CipherSuites {
		name := tls.CipherSuiteName(uint16(cs))
		if name != want[cs] {
			t.Errorf("suite %#04x normalises to %q, want %q", uint16(cs), name, want[cs])
		}
		if id := cipherSuiteIDByName[name]; id != uint16(cs) {
			t.Errorf("%s maps back to %#04x, want %#04x", name, id, uint16(cs))
		}
	}
}
