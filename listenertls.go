package statute

import (
	"crypto/tls"
	"slices"

	"statute.kjanat.dev/resolved"
)

type tlsTransport uint8

const (
	tlsTransportTCP tlsTransport = iota
	tlsTransportQUIC
)

// listenerTLS owns independent certificate and authentication compilers. Both
// transports and the shared HTTP handler consume this same immutable state.
type listenerTLS struct {
	certificates *certRouter
	auth         *clientAuthTable
	policies     *tlsPolicyTable
}

func (s *server) compileListenerTLS(l *resolved.Listener) (*listenerTLS, error) {
	if l.Scheme != schemeHTTPS || l.Redirect != "" {
		return nil, nil
	}
	auth, err := buildClientAuthTable(l)
	if err != nil {
		return nil, err
	}
	certificates, err := s.buildCertRouter(l)
	if err != nil {
		return nil, err
	}
	policies, err := buildTLSPolicyTable(l)
	if err != nil {
		return nil, err
	}
	return &listenerTLS{certificates: certificates, auth: auth, policies: policies}, nil
}

// listenerTLSConfig combines certificate routing and authentication for an
// explicit transport. Only automatic ACME sources on a public TCP listener may
// advertise TLS-ALPN-01, subject to the selected authentication requirements.
func listenerTLSConfig(state *listenerTLS, l *resolved.Listener, transport tlsTransport) *tls.Config {
	cfg := &tls.Config{
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			return strictSNICertificate(state.certificates, state.policies, hello)
		},
		MinVersion: tls.VersionTLS12,
	}
	applyTLSPolicy(cfg, l.TLSPolicy)
	state.auth.defaultAuth.apply(cfg)
	cfg.NextProtos = listenerALPN(state, l, transport, state.auth.defaultAuth)
	if state.auth.scoped || len(l.TLSPolicies) > 0 {
		cfg.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			selected := state.auth.selectPolicy(hello.ServerName)
			handshake := cfg.Clone()
			handshake.GetConfigForClient = nil
			applyTLSPolicy(handshake, state.policies.selectPolicy(hello.ServerName))
			selected.apply(handshake)
			// Preserve net/http's final TCP protocol setup. QUIC's ALPN is
			// explicit and never inferred from the config's protocol list.
			if transport == tlsTransportQUIC {
				handshake.NextProtos = []string{alpnHTTP3}
			} else {
				handshake.NextProtos = clientAuthALPN(cfg.NextProtos, state, l, selected)
			}
			return handshake, nil
		}
	}
	return cfg
}

func listenerALPN(state *listenerTLS, l *resolved.Listener, transport tlsTransport, auth *compiledClientAuth) []string {
	if transport == tlsTransportQUIC {
		return []string{alpnHTTP3}
	}
	protos := []string{alpnHTTP1}
	if l.EnableHTTP2 {
		protos = append([]string{alpnHTTP2}, protos...)
	}
	return clientAuthALPN(protos, state, l, auth)
}

func clientAuthALPN(protos []string, state *listenerTLS, l *resolved.Listener, auth *compiledClientAuth) []string {
	selected := slices.DeleteFunc(slices.Clone(protos), func(p string) bool { return p == alpnACMETLS })
	if state.certificates.hasACMETLS && !l.BehindCloudflare && !auth.requiresCertificate() {
		selected = append(selected, alpnACMETLS)
	}
	return selected
}

// applyTLSPolicy preserves defaults for unset bounds, including the TLS 1.2 floor.
// Both TCP and QUIC use this resolved listener policy.
func applyTLSPolicy(cfg *tls.Config, p *resolved.TLSPolicy) {
	cfg.MinVersion = tls.VersionTLS12
	cfg.MaxVersion = 0
	cfg.CipherSuites = nil
	if p == nil {
		return
	}
	if v, ok := tlsVersionID(p.MinVersion); ok {
		cfg.MinVersion = v
	}
	if v, ok := tlsVersionID(p.MaxVersion); ok {
		cfg.MaxVersion = v
	}
	if len(p.CipherSuites) == 0 {
		return
	}
	suites := make([]uint16, 0, len(p.CipherSuites))
	for _, name := range p.CipherSuites {
		// Unknown names can occur in hand-built schemas. Keep an empty
		// non-nil list when none map, so TLS 1.2 suite selection fails closed.
		if id, ok := cipherSuiteIDByName[name]; ok {
			suites = append(suites, id)
		}
	}
	cfg.CipherSuites = suites
}
