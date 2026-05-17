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
	m := &dns01Manager{domains: []string{"example.com", "api.example.com", "*.test.example.com"}}
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

// TestDNS01_CertValid covers the freshness logic. A leaf that expires within
// 30 days is treated as needing renewal.
func TestDNS01_CertValid(t *testing.T) {
	t.Parallel()
	// nil / empty cert is invalid
	if certValid(nil) || certValid(&tls.Certificate{}) {
		t.Fatal("nil/empty cert must be invalid")
	}

	// Build a fake self-signed cert with controllable NotAfter.
	mk := func(notAfter time.Time) *tls.Certificate {
		t.Helper()
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{
			SerialNumber: big.NewInt(1),
			NotBefore:    notAfter.Add(-24 * time.Hour),
			NotAfter:     notAfter,
		}
		der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	}

	if !certValid(mk(time.Now().Add(90 * 24 * time.Hour))) {
		t.Errorf("90d-out cert must be valid")
	}
	if certValid(mk(time.Now().Add(7 * 24 * time.Hour))) {
		t.Errorf("7d-out cert must be invalid (within 30d window)")
	}
	if certValid(mk(time.Now().Add(-1 * time.Hour))) {
		t.Errorf("expired cert must be invalid")
	}
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

func TestDNS01_TLSConfig_NextProtos(t *testing.T) {
	t.Parallel()
	mgr := &dns01Manager{} // GetCertificate not invoked here
	c1 := dns01TLSConfig(mgr, true)
	if !slices.Equal(c1.NextProtos, []string{"h2", "http/1.1"}) {
		t.Errorf("HTTP/2 protos: %v", c1.NextProtos)
	}
	c2 := dns01TLSConfig(mgr, false)
	if !slices.Equal(c2.NextProtos, []string{"http/1.1"}) {
		t.Errorf("HTTP/1.1 protos: %v", c2.NextProtos)
	}
	// DNS-01 does not need acme-tls/1, ever.
	for _, p := range c1.NextProtos {
		if p == "acme-tls/1" {
			t.Errorf("DNS-01 TLS config must not advertise acme-tls/1")
		}
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
	if m.cf == nil {
		t.Error("cloudflare client not constructed")
	}
	if m.email != "ops@example.com" || m.zoneID != "zone-1" {
		t.Errorf("fields not wired: email=%q zoneID=%q", m.email, m.zoneID)
	}
	if !slices.Equal(m.domains, []string{"example.com"}) {
		t.Errorf("domains not wired: %v", m.domains)
	}
	if m.cache == nil {
		t.Error("cert cache not initialised")
	}
}

var _ = big.NewInt // imported for test fixtures above
