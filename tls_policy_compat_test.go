package statute

import (
	"strings"
	"testing"
)

func TestScopedTLSPolicyMatchesSelectedCertificateKey(t *testing.T) {
	rsa := TLSPolicy{MaxVersion: TLS12, CipherSuites: []CipherSuite{TLSECDHERSAWithAES128GCM}}
	ecdsa := TLSPolicy{MaxVersion: TLS12, CipherSuites: []CipherSuite{TLSECDHEECDSAWithAES128GCM}}
	for _, tc := range []struct {
		name, domain, policyHost, key string
		policy                        TLSPolicy
		other                         []ListenerOption
		bad                           bool
	}{
		{"exact ECDSA with RSA policy", "api.example", "api.example", "EC256", rsa, nil, true},
		{"exact RSA with ECDSA policy", "api.example", "api.example", "RSA2048", ecdsa, nil, true},
		{"wildcard source exact policy", "*.example", "api.example", "EC256", rsa, nil, true},
		{"exact source wildcard policy", "api.example", "*.example", "EC256", rsa, nil, true},
		{"wildcard source wildcard policy", "*.example", "*.example", "EC256", rsa, nil, true},
		{"matching key", "api.example", "api.example", "RSA2048", rsa, nil, false},
		{"unrelated policy", "api.example", "other.example", "EC256", rsa, nil, false},
		{"static exact shadows wildcard", "*.example", "api.example", "EC256", rsa, []ListenerOption{StaticTLSFor("api.example", "unused.pem", "unused.key")}, false},
		{"scoped override repairs default", "api.example", "api.example", "EC256", ecdsa, []ListenerOption{rsa}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, http01 := range []bool{false, true} {
				if http01 && strings.HasPrefix(tc.domain, "*.") {
					continue
				}
				source := AutoTLS(tc.domain).Email("ops@example.test").Storage(t.TempDir()).KeyType(tc.key)
				if http01 {
					source.HTTP01()
				} else {
					source.CloudflareDNS01("test-token")
				}
				opts := append([]ListenerOption{source, TLSPolicyFor([]string{tc.policyHost}, tc.policy)}, tc.other...)
				cfg := tlsRouterConfig(opts...)
				if http01 {
					cfg = withPlainHTTP(cfg)
				}
				_, err := Resolve(cfg)
				if (err != nil) != tc.bad {
					t.Fatalf("HTTP01=%t resolve err=%v want rejection=%t", http01, err, tc.bad)
				}
			}
		})
	}
}
