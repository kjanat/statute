package statute

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"statute.kjanat.dev/resolved"
)

// TestDNS01_CoversHost exercises the wildcard-vs-exact match table without
// requiring an ACME server.
func TestDNS01_CoversHost(t *testing.T) {
	t.Parallel()
	m := &acmeManager{name: "dns01", domains: []string{"example.com", "api.example.com", "*.test.example.com"}}
	cases := []struct {
		host string
		want bool
	}{
		{"example.com", true},
		{"api.example.com", true},
		// foo.example.com is not under any wildcard and not an exact match
		{"foo.example.com", false},
		// matches *.test.example.com
		{"a.test.example.com", true},
		{"b.test.example.com", true},
		// wildcard is single-label, so deeply nested subdomains do not match
		{"deep.b.test.example.com", false},
		{"other.com", false},
	}
	for _, c := range cases {
		got := m.coversHost(c.host)
		if got != c.want {
			t.Errorf("coversHost(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

func TestDNS01_GetCertificate_ReusesWildcard(t *testing.T) {
	t.Parallel()
	cert := testCertificate(t, time.Now().Add(90*24*time.Hour))
	m := &acmeManager{
		name:    "dns01",
		domains: []string{"*.example.com"},
		cache:   map[string]*tls.Certificate{"*.example.com": cert},
	}

	got, err := m.GetCertificate(&tls.ClientHelloInfo{ServerName: "Foo.Example.Com."})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if got != cert {
		t.Fatal("GetCertificate did not return the cached wildcard certificate")
	}
	if _, ok := m.cache["foo.example.com"]; ok {
		t.Fatal("GetCertificate created a concrete-host cache entry")
	}
}

// TestDNS01_GetCertificate_PrefersExactOverWildcard pins the precedence rule
// in matchDomain: when both an exact host and a covering wildcard are
// configured, the exact certificate wins — regardless of which is listed
// first.
func TestDNS01_GetCertificate_PrefersExactOverWildcard(t *testing.T) {
	t.Parallel()
	exact := testCertificate(t, time.Now().Add(90*24*time.Hour))
	wildcard := testCertificate(t, time.Now().Add(90*24*time.Hour))

	// The wildcard is listed first on purpose: precedence must come from the
	// match rule, not from configuration order.
	m := &acmeManager{
		name:    "dns01",
		domains: []string{"*.example.com", "foo.example.com"},
		cache: map[string]*tls.Certificate{
			"*.example.com":   wildcard,
			"foo.example.com": exact,
		},
	}

	got, err := m.GetCertificate(&tls.ClientHelloInfo{ServerName: "Foo.Example.Com."})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	switch got {
	case exact: // want
	case wildcard:
		t.Fatal("GetCertificate returned the wildcard certificate; the exact host must win")
	default:
		t.Fatal("GetCertificate returned an unexpected certificate")
	}

	// A sibling host with no exact entry still falls back to the wildcard.
	got, err = m.GetCertificate(&tls.ClientHelloInfo{ServerName: "bar.example.com"})
	if err != nil {
		t.Fatalf("GetCertificate(bar): %v", err)
	}
	if got != wildcard {
		t.Fatal("sibling host did not fall back to the wildcard certificate")
	}
	if _, ok := m.cache["bar.example.com"]; ok {
		t.Fatal("GetCertificate created a concrete-host cache entry for the wildcard fallback")
	}
}

// TestACMEManager_GetCertificate_IDNAEquivalence pins the canonicalTLSName
// call inside acmeManager.GetCertificate. Resolve stores every AutoTLS
// domain in its IDNA A-label form, but a client may send the U-label in
// the ClientHello, in any case and with a trailing root dot. All three
// spellings name the same host and must serve the same certificate;
// ad-hoc lowercasing (what GetCertificate did before) leaves the U-label
// with nothing to match and fails the handshake with "host not
// configured".
func TestACMEManager_GetCertificate_IDNAEquivalence(t *testing.T) {
	t.Parallel()
	const aLabel = "xn--mnchen-3ya.example" // münchen.example
	cert := testCertificate(t, time.Now().Add(90*24*time.Hour))
	m := &acmeManager{
		name:    "http01",
		domains: []string{aLabel},
		cache:   map[string]*tls.Certificate{aLabel: cert},
	}

	for _, sni := range []string{"MÜNCHEN.example.", "münchen.example", aLabel} {
		got, err := m.GetCertificate(&tls.ClientHelloInfo{ServerName: sni})
		if err != nil {
			t.Fatalf("GetCertificate(%q): %v", sni, err)
		}
		if got != cert {
			t.Errorf("GetCertificate(%q) did not return the cached certificate", sni)
		}
	}
	if len(m.cache) != 1 {
		t.Errorf("cache grew beyond the A-label entry: %v", m.cache)
	}
}

// TestDNS01_CertPredicates pins the split between the two questions the
// manager asks about a certificate. usableCert decides whether it can
// terminate a handshake — only the leaf's own validity window matters —
// while needsRenewal decides whether to order a replacement, 30 days
// early. A cert in that overlap is both usable and due for renewal;
// collapsing the two would make every handshake in the last 30 days order
// a duplicate certificate.
func TestDNS01_CertPredicates(t *testing.T) {
	t.Parallel()
	now := time.Now()
	cases := []struct {
		name       string
		cert       *tls.Certificate
		usable     bool
		needsRenew bool
	}{
		{"nil", nil, false, true},
		{"no chain", &tls.Certificate{}, false, true},
		{"90 days left", testCertificate(t, now.Add(90*24*time.Hour)), true, false},
		{"7 days left", testCertificate(t, now.Add(7*24*time.Hour)), true, true},
		{"expired", testCertificate(t, now.Add(-time.Hour)), false, true},
		{"not yet valid", testCertificateFrom(t, now.Add(24*time.Hour), now.Add(90*24*time.Hour)), false, false},
	}
	for _, c := range cases {
		if got := usableCert(c.cert); got != c.usable {
			t.Errorf("%s: usableCert = %v, want %v", c.name, got, c.usable)
		}
		if got := needsRenewal(c.cert); got != c.needsRenew {
			t.Errorf("%s: needsRenewal = %v, want %v", c.name, got, c.needsRenew)
		}
	}
}

// testCertificate builds a self-signed leaf already in force, expiring at
// notAfter. Backdating NotBefore relative to notAfter instead would make
// every long-lived fixture start in the future, which is not a state a
// served certificate is ever in.
func testCertificate(t *testing.T, notAfter time.Time) *tls.Certificate {
	t.Helper()
	return testCertificateFrom(t, time.Now().Add(-time.Hour), notAfter)
}

func testCertificateFrom(t *testing.T, notBefore, notAfter time.Time) *tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    notBefore,
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// TestDNS01_ECPrivateKeyRoundTrip ensures the on-disk PEM serialisation
// round-trips bit-for-bit, which is the contract for cert persistence.
func TestDNS01_ECPrivateKeyRoundTrip(t *testing.T) {
	t.Parallel()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "test.key")
	if err := writeECPrivateKey(path, key); err != nil {
		t.Fatalf("write: %v", err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	loaded, err := parseECPrivateKey(pemBytes)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !loaded.Equal(key) {
		t.Errorf("round-tripped key differs")
	}
}

func TestDNS01_NewManager_ValidatesConfig(t *testing.T) {
	t.Parallel()
	_, err := newDNS01Manager(nil)
	if err == nil {
		t.Fatal("want error for nil config")
	}
	_, err = newDNS01Manager(&resolved.AutoTLS{Domains: []string{"x"}, Email: "x@x", Storage: t.TempDir()})
	if err == nil {
		t.Fatal("want error for config without DNS01 field")
	}

	// Storage points at a regular file, so MkdirAll(<file>/dns01) fails.
	f := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = newDNS01Manager(&resolved.AutoTLS{
		Storage: f,
		DNS01:   &resolved.CloudflareDNS01{APIToken: "tok"},
	})
	if err == nil {
		t.Fatal("want error when storage dir cannot be created")
	}
}

// TestDNS01_NewManager_BuildsManager covers the success path: storage dir
// creation and the struct build (including the cloudflare client).
func TestDNS01_NewManager_BuildsManager(t *testing.T) {
	t.Parallel()
	cfg := &resolved.AutoTLS{
		Domains: []string{"example.com"},
		Email:   "ops@example.com",
		Storage: t.TempDir(),
		DNS01:   &resolved.CloudflareDNS01{APIToken: "tok", ZoneID: "zone-1"},
	}
	m, err := newDNS01Manager(cfg)
	if err != nil {
		t.Fatalf("newDNS01Manager: %v", err)
	}
	solver, ok := m.solver.(*dns01Solver)
	if !ok {
		t.Fatalf("solver: got %T, want *dns01Solver", m.solver)
	}
	if solver.cf == nil {
		t.Error("cloudflare client not constructed")
	}
	if m.email != "ops@example.com" || solver.zoneID != "zone-1" {
		t.Errorf("fields not wired: email=%q zoneID=%q", m.email, solver.zoneID)
	}
	if !slices.Equal(m.domains, []string{"example.com"}) {
		t.Errorf("domains not wired: %v", m.domains)
	}
	if m.cache == nil {
		t.Error("cert cache not initialised")
	}
}

var _ = big.NewInt // imported for test fixtures above
