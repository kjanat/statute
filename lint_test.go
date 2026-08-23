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
