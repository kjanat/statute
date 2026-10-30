package statute

import (
	"crypto/tls"
	"testing"

	"statute.kjanat.dev/resolved"
)

func TestScopedTLSPolicyReplacesDefaultBounds(t *testing.T) {
	l := &resolved.Listener{TLSPolicy: &resolved.TLSPolicy{MaxVersion: "1.2", CipherSuites: []string{"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256"}}, TLSPolicies: []resolved.HostTLSPolicy{{Hosts: []string{"modern.example"}, Policy: resolved.TLSPolicy{MinVersion: "1.3"}}}}
	auth, err := buildClientAuthTable(l)
	if err != nil {
		t.Fatal(err)
	}
	policies, err := buildTLSPolicyTable(l)
	if err != nil {
		t.Fatal(err)
	}
	for _, transport := range []tlsTransport{tlsTransportTCP, tlsTransportQUIC} {
		state := &listenerTLS{certificates: &certRouter{}, auth: auth, policies: policies}
		cfg := listenerTLSConfig(state, l, transport)
		selected, err := cfg.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "modern.example"})
		if err != nil {
			t.Fatal(err)
		}
		if selected.MinVersion != tls.VersionTLS13 || selected.MaxVersion != 0 || selected.CipherSuites != nil {
			t.Fatalf("host policy inherited defaults: min=%x max=%x suites=%v", selected.MinVersion, selected.MaxVersion, selected.CipherSuites)
		}
		fallback, err := cfg.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "other.example"})
		if err != nil {
			t.Fatal(err)
		}
		assertFallbackTLSPolicy(t, fallback)
	}
}

func assertFallbackTLSPolicy(t *testing.T, cfg *tls.Config) {
	t.Helper()
	if cfg.MaxVersion != tls.VersionTLS12 || len(cfg.CipherSuites) != 1 {
		t.Fatalf("default policy changed: %v", cfg)
	}
}
