package statute

import (
	"slices"
	"strings"
	"testing"
)

// findingCodes returns the sorted set of finding codes in the result.
func findingCodes(findings []Finding) []string {
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, f.Code)
	}
	slices.Sort(out)
	return out
}

func TestLint_AllRulesFireOnDeliberatelyBadConfig(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Listeners: Listeners{
			HTTP(":80"), // No HTTPS — AUTH001 trigger
		},
		Upstreams: Upstreams{
			"single": Pool{Backends: []Backend{{Address: "127.0.0.1:1"}}}, // LB001 + HC001
		},
		Routes: Routes{
			Match("/*").ProxyTo("single").With(
				RateLimit("30/min"), // RL001 (0.5/s < 1/s)
				BasicAuth("realm", map[string]string{
					"alice": "$2a$10$HwrzUQtDrRX0/09su3BahezCIqD.f4HjCkYD5b9w8gl4eUkPJzCyu",
				}), // AUTH001
			),
		},
		Defaults: Defaults{
			ReadHeaderTimeout: "0s", // RHT001 path uses the resolved value of zero — overridden below
		},
		Shutdown: Shutdown{GracePeriod: "1s"}, // SHUT001
		// Observability empty — OBS001 and OBS002
	}
	// The framework defaults ReadHeaderTimeout to 5s when the user passes "";
	// to make RHT001 fire we force the resolved value to zero explicitly.
	r, err := Resolve(cfg)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	r.Defaults.ReadHeaderTimeout = 0
	all := make([]Finding, 0, len(lintRules))
	for _, rule := range lintRules {
		all = append(all, rule(r)...)
	}

	codes := findingCodes(all)
	expected := []string{"AUTH001", "HC001", "LB001", "OBS001", "OBS002", "RHT001", "RL001", "SHUT001"}
	for _, code := range expected {
		if !slices.Contains(codes, code) {
			t.Errorf("expected finding %s not raised; got %v", code, codes)
		}
	}
}

func TestLint_CleanConfigHasNoErrors(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Listeners: Listeners{
			HTTP(":80").RedirectTo("https"),
			HTTPS(":443",
				StaticTLS("/etc/cert.pem", "/etc/key.pem"),
				HTTP2(),
			),
		},
		Upstreams: Upstreams{
			"api": Pool{
				Backends: []Backend{
					{Address: "10.0.0.1:8080"},
					{Address: "10.0.0.2:8080"},
				},
				Strategy: RoundRobin,
				HealthCheck: HealthCheck{
					Path: "/healthz", Interval: "10s",
				},
			},
		},
		Routes: Routes{
			Match("/*").ProxyTo("api").With(Timeout("30s")),
		},
		Defaults: Defaults{ReadHeaderTimeout: "5s"},
		Observability: Observability{
			AccessLog: JSONLog(Stdout),
			Metrics:   Prometheus(":9090", "/metrics"),
		},
		Shutdown: Shutdown{GracePeriod: "30s"},
	}
	findings, err := Lint(cfg)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	for _, f := range findings {
		if f.Severity == SeverityError {
			t.Errorf("unexpected error finding on clean config: %s", f)
		}
	}
}

func TestLint_TLS001OnTmpStorage(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Listeners: Listeners{
			HTTPS(":443",
				AutoTLS("example.com").Email("ops@example.com").Storage("/tmp/badcerts"),
				HTTP2(),
			),
			HTTPS(":8443",
				AutoTLS("good.example.com").Email("ops@example.com").Storage("/var/lib/certs"),
			),
		},
		Upstreams: Upstreams{"a": Pool{Backends: []Backend{{Address: "127.0.0.1:1"}, {Address: "127.0.0.1:2"}}}},
		Routes:    Routes{Match("/*").ProxyTo("a")},
		Defaults:  Defaults{ReadHeaderTimeout: "5s"},
		Shutdown:  Shutdown{GracePeriod: "30s"},
	}
	findings, err := Lint(cfg)
	if err != nil {
		t.Fatal(err)
	}
	hit := false
	for _, f := range findings {
		if f.Code == "TLS001" {
			hit = true
			if !strings.Contains(f.Message, "/tmp") {
				t.Errorf("TLS001 message missing /tmp reference: %s", f.Message)
			}
		}
	}
	if !hit {
		t.Errorf("TLS001 did not fire on /tmp storage; findings=%v", findingCodes(findings))
	}
}

func TestFinding_StringFormat(t *testing.T) {
	t.Parallel()
	f := Finding{Severity: SeverityError, Code: "X001", Message: "bad", Path: "p"}
	got := f.String()
	for _, w := range []string{"error", "X001", "bad", "p"} {
		if !strings.Contains(got, w) {
			t.Errorf("String missing %q: %s", w, got)
		}
	}
}

// acmeLintConfig wraps the given listeners with the pool, route, and
// defaults a clean Lint run needs, so a TLS003 case only differs in its
// ACME sources.
func acmeLintConfig(ls ...*Listener) Config {
	return Config{
		Listeners: ls,
		Upstreams: Upstreams{"a": Pool{Backends: []Backend{{Address: "127.0.0.1:1"}, {Address: "127.0.0.1:2"}}}},
		Routes:    Routes{Match("/*").ProxyTo("a")},
		Defaults:  Defaults{ReadHeaderTimeout: "5s"},
		Shutdown:  Shutdown{GracePeriod: "30s"},
	}
}

// TestLint_TLS003DuplicateACMEOrders — a domain issued by two certificate
// managers is reported once, at the source that introduced the second
// manager. Two automatic sources share the one autocert manager, so that
// shape stays silent.
func TestLint_TLS003DuplicateACMEOrders(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		cfg      Config
		wantPath string // empty means the rule must not fire
	}{
		{
			// One storage root on both: automatic sources must agree on it
			// (buildAutocertManager rejects a mismatch at server start), so
			// this is the shape that actually runs — and it is legal.
			"two automatic sources share the autocert manager",
			acmeLintConfig(
				HTTPS(":443", AutoTLS("dup.example").Email("ops@example.com").Storage("/var/lib/a")),
				HTTPS(":8443", AutoTLS("dup.example").Email("ops@example.com").Storage("/var/lib/a")),
			),
			"",
		},
		{
			"pinned twice with distinct storage roots",
			acmeLintConfig(
				HTTPS(":443", AutoTLS("dup.example").Email("ops@example.com").Storage("/var/lib/a").CloudflareDNS01("tok")),
				HTTPS(":8443", AutoTLS("dup.example").Email("ops@example.com").Storage("/var/lib/b").CloudflareDNS01("tok")),
			),
			"listeners[1].auto_tls[0]",
		},
		{
			"pinned twice with different challenge kinds under one root",
			acmeLintConfig(
				HTTPS(":443", AutoTLS("dup.example").Email("ops@example.com").Storage("/var/lib/a").HTTP01()),
				HTTPS(":8443", AutoTLS("dup.example").Email("ops@example.com").Storage("/var/lib/a").CloudflareDNS01("tok")),
				HTTP(":80"),
			),
			"listeners[1].auto_tls[0]",
		},
		{
			"pinned on one source and automatic on another",
			acmeLintConfig(
				HTTPS(":443", AutoTLS("dup.example").Email("ops@example.com").Storage("/var/lib/a")),
				HTTPS(":8443", AutoTLS("dup.example").Email("ops@example.com").Storage("/var/lib/b").HTTP01()),
				HTTP(":80"),
			),
			"listeners[1].auto_tls[0]",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			findings, err := Lint(c.cfg)
			if err != nil {
				t.Fatalf("Lint: %v", err)
			}
			assertTLS003(t, findings, c.wantPath)
		})
	}
}

// assertTLS003 checks the TLS003 findings in the result: exactly one at
// wantPath, or none at all when wantPath is empty.
func assertTLS003(t *testing.T, findings []Finding, wantPath string) {
	t.Helper()
	var hits []Finding
	for _, f := range findings {
		if f.Code == "TLS003" {
			hits = append(hits, f)
		}
	}
	if wantPath == "" {
		if len(hits) != 0 {
			t.Fatalf("TLS003 must not fire here; got %v", hits)
		}
		return
	}
	if len(hits) != 1 {
		t.Fatalf("TLS003: got %d findings, want 1: %v", len(hits), hits)
	}
	f := hits[0]
	if f.Severity != SeverityWarning {
		t.Errorf("severity: got %q, want %q", f.Severity, SeverityWarning)
	}
	if f.Path != wantPath {
		t.Errorf("path: got %q, want %q", f.Path, wantPath)
	}
	if !strings.Contains(f.Message, "dup.example") {
		t.Errorf("message does not name the domain: %s", f.Message)
	}
}

// TestLint_TLS004RSAOnlyAutocert — a TLS 1.2-capped, RSA-only suite policy
// on a listener with automatic ACME sources is a warning, not a resolve
// error: autocert issues RSA leaves to clients without ECDSA support, so
// the listener works for exactly that population. The finding mentions
// the TLS-ALPN-01 hazard only where the listener advertises acme-tls/1.
func TestLint_TLS004RSAOnlyAutocert(t *testing.T) {
	t.Parallel()
	rsaOnly := TLSPolicy{
		MaxVersion:   TLS12,
		CipherSuites: []CipherSuite{TLSECDHERSAWithAES128GCM},
	}
	auto := func() *AutoTLSConfig {
		return AutoTLS("legacy.example").Email("ops@example.com").Storage("/var/lib/a")
	}
	cases := []struct {
		name     string
		cfg      Config
		wantPath string // empty means the rule must not fire
		wantALPN bool   // finding mentions the TLS-ALPN-01 challenge cert
	}{
		{
			"RSA-only cap on an automatic source",
			acmeLintConfig(HTTPS(":443", auto(), rsaOnly)),
			"listeners[0].tls_policy",
			true,
		},
		{
			// Cloudflare's edge terminates acme-tls/1, so the challenge
			// hazard disappears — the per-client leaf hazard stays.
			"behind Cloudflare the ALPN clause drops",
			acmeLintConfig(HTTPS(":443", auto(), rsaOnly, BehindCloudflare())),
			"listeners[0].tls_policy",
			false,
		},
		{
			"an ECDSA suite silences the rule",
			acmeLintConfig(HTTPS(":443", auto(), TLSPolicy{
				MaxVersion: TLS12,
				CipherSuites: []CipherSuite{
					TLSECDHERSAWithAES128GCM,
					TLSECDHEECDSAWithAES128GCM,
				},
			})),
			"",
			false,
		},
		{
			"an uncapped policy silences the rule",
			acmeLintConfig(HTTPS(":443", auto(), TLSPolicy{
				CipherSuites: []CipherSuite{TLSECDHERSAWithAES128GCM},
			})),
			"",
			false,
		},
		{
			"static sources are not judged",
			acmeLintConfig(HTTPS(":443", StaticTLS("cert.pem", "key.pem"), rsaOnly)),
			"",
			false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			findings, err := Lint(c.cfg)
			if err != nil {
				t.Fatalf("Lint: %v", err)
			}
			assertTLS004(t, findings, c.wantPath, c.wantALPN)
		})
	}
}

// assertTLS004 checks the TLS004 findings: exactly one at wantPath whose
// ALPN mention matches wantALPN, or none at all when wantPath is empty.
func assertTLS004(t *testing.T, findings []Finding, wantPath string, wantALPN bool) {
	t.Helper()
	var hits []Finding
	for _, f := range findings {
		if f.Code == "TLS004" {
			hits = append(hits, f)
		}
	}
	if wantPath == "" {
		if len(hits) != 0 {
			t.Fatalf("TLS004 must not fire here; got %v", hits)
		}
		return
	}
	if len(hits) != 1 {
		t.Fatalf("TLS004: got %d findings, want 1: %v", len(hits), hits)
	}
	f := hits[0]
	if f.Severity != SeverityWarning {
		t.Errorf("severity: got %q, want %q", f.Severity, SeverityWarning)
	}
	if f.Path != wantPath {
		t.Errorf("path: got %q, want %q", f.Path, wantPath)
	}
	if got := strings.Contains(f.Message, "TLS-ALPN-01"); got != wantALPN {
		t.Errorf("ALPN mention: got %v, want %v: %s", got, wantALPN, f.Message)
	}
}
