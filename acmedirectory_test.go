package statute

import (
	"strings"
	"testing"

	"golang.org/x/crypto/acme"

	"statute.kjanat.dev/resolved"
)

// TestResolveACMEDirectoryDefault — the resolved model always carries the
// directory actually used, so an unset surface value must come back as
// Let's Encrypt production, not as empty.
func TestResolveACMEDirectoryDefault(t *testing.T) {
	t.Parallel()
	r := mustResolve(t, multiListenerConfig(
		HTTPS(":443", AutoTLS("a.example").Email("x@x").Storage("/v")),
	))
	got := r.Listeners[0].AutoTLSSources[0].Directory
	if got != acme.LetsEncryptURL {
		t.Errorf("directory: got %q, want the Let's Encrypt default", got)
	}
}

func TestResolveACMEDirectoryOverride(t *testing.T) {
	t.Parallel()
	r := mustResolve(t, multiListenerConfig(
		HTTPS(":443", AutoTLS("a.example").Email("x@x").Storage("/v").Directory("https://pebble:14000/dir")),
	))
	got := r.Listeners[0].AutoTLSSources[0].Directory
	if got != "https://pebble:14000/dir" {
		t.Errorf("directory: got %q", got)
	}
}

func TestResolveACMEDirectoryInvalid(t *testing.T) {
	t.Parallel()
	for _, dir := range []string{"pebble:14000", "/dir", "ftp://ca.example/dir", "https://"} {
		_, err := Resolve(multiListenerConfig(
			HTTPS(":443", AutoTLS("a.example").Email("x@x").Storage("/v").Directory(dir)),
		))
		if err == nil || !strings.Contains(err.Error(), "directory") {
			t.Errorf("directory %q: got %v, want directory validation error", dir, err)
		}
	}
}

// TestPinnedACMEAccountDirectoryMismatch — pinned sources sharing
// <storage>/<challenge>/account.key share one account key, and one key
// cannot be registered with two CAs; mirrors the email-mismatch rule.
func TestPinnedACMEAccountDirectoryMismatch(t *testing.T) {
	t.Parallel()
	_, err := Resolve(multiListenerConfig(
		HTTPS(":443", AutoTLS("a.example").Email("x@x").Storage("/v").CloudflareDNS01("tok")),
		HTTPS(":8443", AutoTLS("b.example").Email("x@x").Storage("/v").CloudflareDNS01("tok").Directory("https://ca.example/dir")),
	))
	if err == nil || !strings.Contains(err.Error(), "directory mismatch") {
		t.Errorf("divergent directory on one ACME account: %v", err)
	}

	// Distinct challenge subdirectories are distinct accounts and may
	// point at different CAs.
	mustResolve(t, multiListenerConfig(
		HTTP(":80"),
		HTTPS(":443", AutoTLS("a.example").Email("x@x").Storage("/v").CloudflareDNS01("tok")),
		HTTPS(":8443", AutoTLS("b.example").Email("x@x").Storage("/v").HTTP01().Directory("https://ca.example/dir")),
	))
}

func TestBuildAutocertManagerDirectory(t *testing.T) {
	t.Parallel()
	src := func(directory string) *resolved.Listener {
		return &resolved.Listener{
			Addr: ":443", Scheme: "https",
			AutoTLSSources: []*resolved.AutoTLS{{
				Domains: []string{"a.example"}, Email: "x@x", Storage: "/v", Directory: directory,
			}},
		}
	}

	m, err := buildAutocertManager([]*resolved.Listener{src(acme.LetsEncryptURL)})
	if err != nil {
		t.Fatal(err)
	}
	if m.Client != nil {
		t.Errorf("client: got non-nil for the default directory; autocert's own default should apply")
	}

	m, err = buildAutocertManager([]*resolved.Listener{src("https://ca.example/dir")})
	if err != nil {
		t.Fatal(err)
	}
	if m.Client == nil || m.Client.DirectoryURL != "https://ca.example/dir" {
		t.Errorf("client: got %+v, want DirectoryURL https://ca.example/dir", m.Client)
	}

	_, err = buildAutocertManager([]*resolved.Listener{src(acme.LetsEncryptURL), src("https://ca.example/dir")})
	if err == nil || !strings.Contains(err.Error(), "directory mismatch") {
		t.Errorf("divergent directories across automatic sources: %v", err)
	}
}

// TestNewACMEManagerDirectory — the public knob must reach the client the
// in-tree manager builds; hand-built fixtures without a directory keep the
// Let's Encrypt default.
func TestNewACMEManagerDirectory(t *testing.T) {
	t.Parallel()
	cfg := &resolved.AutoTLS{
		Domains:   []string{"a.example"},
		Email:     "x@x",
		Storage:   t.TempDir(),
		Directory: "https://pebble:14000/dir",
	}
	m, err := newACMEManager(cfg, "http01", nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.directoryURL != "https://pebble:14000/dir" {
		t.Errorf("directoryURL: got %q", m.directoryURL)
	}

	cfg.Directory = ""
	m, err = newACMEManager(cfg, "http01", nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.directoryURL != acme.LetsEncryptURL {
		t.Errorf("directoryURL: got %q, want the Let's Encrypt fallback", m.directoryURL)
	}
}

func TestLint_ACMEDirectoryFindings(t *testing.T) {
	t.Parallel()
	base := func(directory string) Config {
		cfg := multiListenerConfig(
			HTTPS(":443", AutoTLS("a.example").Email("x@x").Storage("/var/lib/acme").Directory(directory)),
		)
		return cfg
	}
	codes := func(t *testing.T, cfg Config) map[string]bool {
		t.Helper()
		findings, err := Lint(cfg)
		if err != nil {
			t.Fatal(err)
		}
		out := make(map[string]bool, len(findings))
		for _, f := range findings {
			out[f.Code] = true
		}
		return out
	}

	got := codes(t, base("http://pebble:14000/dir"))
	if !got["TLS005"] {
		t.Errorf("plain-HTTP directory: TLS005 missing (got %v)", got)
	}

	got = codes(t, base(acmeStagingURL))
	if !got["TLS006"] {
		t.Errorf("staging directory: TLS006 missing (got %v)", got)
	}

	got = codes(t, base("https://ca.internal/dir"))
	if !got["TLS007"] {
		t.Errorf("non-LE directory: TLS007 missing (got %v)", got)
	}

	got = codes(t, base(""))
	if got["TLS005"] || got["TLS006"] || got["TLS007"] {
		t.Errorf("default directory: unexpected directory findings (got %v)", got)
	}
}
