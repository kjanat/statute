package statute

import (
	"crypto/tls"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/net/idna"

	"statute.kjanat.dev/resolved"
)

// canonicalTLSName reduces a configured or ClientHello TLS name to the one
// form certificate routing compares: trimmed, one trailing dot stripped,
// and the IDNA A-label lookup form — the same canonicalisation autocert
// applies — falling back to plain lowercasing for names the IDNA profile
// rejects, so unusual-but-working static hostnames keep matching. A
// leading "*." wildcard marker is preserved and its suffix canonicalised
// on its own. Resolve applies it to every AutoTLS domain and static host,
// and the router applies it to every SNI lookup, so both sides always
// agree.
func canonicalTLSName(name string) string {
	n := strings.TrimSuffix(strings.TrimSpace(name), ".")
	prefix := ""
	if strings.HasPrefix(n, "*.") {
		prefix, n = "*.", n[2:]
	}
	if a, err := idna.Lookup.ToASCII(n); err == nil {
		n = a
	}
	return prefix + strings.ToLower(n)
}

// certGetter resolves the certificate for one TLS handshake. Both
// autocert.Manager.GetCertificate and dns01Manager.GetCertificate have this
// shape; static sources close over a key pair loaded at startup.
type certGetter func(*tls.ClientHelloInfo) (*tls.Certificate, error)

// certRouter selects the TLS source for a handshake by SNI hostname. One
// router serves one listener and holds every source the listener declared:
// exact names win over wildcard patterns regardless of declaration order,
// and the hostless static fallback catches everything else, including
// clients that send no SNI at all. Resolve-time validation guarantees no
// name is claimed twice, and a wildcard covers exactly one extra label, so
// at most one pattern can match any host — selection is deterministic.
type certRouter struct {
	exact     map[string]certGetter // lowercased exact SNI name -> source
	wildcards map[string]certGetter // lowercased "*.suffix" pattern -> source
	fallback  certGetter            // hostless static source; nil when absent

	// hasACMETLS records that an HTTP-01 ACME source is present, so the
	// listener must advertise the acme-tls/1 ALPN protocol for TLS-ALPN-01
	// challenges. The challenge hello carries the challenged domain as SNI,
	// which routes it to the autocert source like any other handshake.
	hasACMETLS bool
}

// buildCertRouter indexes the listener's resolved TLS sources against their
// runtime certificate managers: the shared autocert manager for HTTP-01
// sources, the per-source dns01Manager for DNS-01 sources. Static key pairs
// load here so a bad path fails at construction, not mid-handshake.
func (s *server) buildCertRouter(l *resolved.Listener) (*certRouter, error) {
	cr := &certRouter{
		exact:     make(map[string]certGetter),
		wildcards: make(map[string]certGetter),
	}
	if err := cr.indexACMESources(s, l.AutoTLSSources); err != nil {
		return nil, err
	}
	if err := cr.indexStaticSources(l.StaticTLSSources); err != nil {
		return nil, err
	}
	if len(cr.exact) == 0 && len(cr.wildcards) == 0 && cr.fallback == nil {
		return nil, errors.New("https listener has no TLS material")
	}
	return cr, nil
}

func (cr *certRouter) indexACMESources(s *server, sources []*resolved.AutoTLS) error {
	for _, a := range sources {
		var g certGetter
		switch {
		// Manager selection keys on the DNS-01 config itself, matching
		// initDNS01Managers; Challenge is its resolved mirror and governs
		// only the TLS-ALPN posture below.
		case a.DNS01 != nil:
			dm := s.dns01Managers[a]
			if dm == nil {
				return errors.New("auto_tls: dns01 manager not initialised")
			}
			g = dm.GetCertificate
		case a.Challenge == resolved.ChallengeHTTP01:
			if s.autocertMgr == nil {
				return errors.New("auto_tls: manager not initialised")
			}
			g = pinHTTP01(s.autocertMgr.GetCertificate)
		default: // ChallengeAuto
			if s.autocertMgr == nil {
				return errors.New("auto_tls: manager not initialised")
			}
			g = s.autocertMgr.GetCertificate
			cr.hasACMETLS = true
		}
		for _, d := range a.Domains {
			cr.add(d, g)
		}
	}
	return nil
}

// pinHTTP01 wraps an autocert getter for a source pinned to the HTTP-01
// challenge. The shared autocert manager always attempts TLS-ALPN-01 first
// when the CA offers it, so pinning is enforced from the outside: the
// listener does not advertise acme-tls/1 for this source (hasACMETLS stays
// false), and — because another source on the same listener may still
// advertise it — a challenge hello that reaches the source anyway is
// refused, failing the manager's TLS-ALPN attempt so issuance falls back
// to HTTP-01.
func pinHTTP01(g certGetter) certGetter {
	return func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if isACMEChallengeHello(hello) {
			return nil, errors.New("tls: source pinned to HTTP-01; TLS-ALPN-01 challenge refused")
		}
		return g(hello)
	}
}

// isACMEChallengeHello mirrors autocert's own detection of a TLS-ALPN-01
// challenge probe: the CA offers exactly the ACME ALPN protocol.
func isACMEChallengeHello(hello *tls.ClientHelloInfo) bool {
	return len(hello.SupportedProtos) == 1 && hello.SupportedProtos[0] == alpnACMETLS
}

func (cr *certRouter) indexStaticSources(sources []*resolved.StaticTLS) error {
	for _, st := range sources {
		cert, err := tls.LoadX509KeyPair(st.CertFile, st.KeyFile)
		if err != nil {
			return fmt.Errorf("static_tls: load %s: %w", st.CertFile, err)
		}
		g := func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil }
		if st.Host == "" {
			cr.fallback = g
			continue
		}
		cr.add(st.Host, g)
	}
	return nil
}

func (cr *certRouter) add(pattern string, g certGetter) {
	p := canonicalTLSName(pattern)
	if strings.HasPrefix(p, "*.") {
		cr.wildcards[p] = g
		return
	}
	cr.exact[p] = g
}

// GetCertificate satisfies tls.Config.GetCertificate. A wildcard pattern
// covers exactly one extra label — "*.bar.example" matches foo.bar.example
// but not baz.foo.bar.example — matching dns01Manager.matchDomain and RFC
// 6125 certificate semantics, so the candidate pattern for a host is a
// single map lookup on its first label replaced by "*".
func (cr *certRouter) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	host := canonicalTLSName(hello.ServerName)
	if g, ok := cr.exact[host]; ok {
		return g(hello)
	}
	if _, rest, ok := strings.Cut(host, "."); ok && rest != "" {
		if g, ok := cr.wildcards["*."+rest]; ok {
			return g(hello)
		}
	}
	if cr.fallback != nil {
		return cr.fallback(hello)
	}
	if host == "" {
		return nil, errors.New("tls: no SNI hostname sent and listener has no fallback certificate")
	}
	return nil, fmt.Errorf("tls: no TLS source covers %q on this listener", host)
}

// certRouterTLSConfig builds the listener's *tls.Config around the router.
// ALPN advertises h2 when HTTP/2 is enabled, and acme-tls/1 when a
// ChallengeAuto ACME source may answer TLS-ALPN-01 challenges — sources
// pinned to HTTP-01 or DNS-01 never advertise it, and neither does a
// listener behind Cloudflare, whose edge terminates TLS and cannot forward
// the challenge protocol.
func certRouterTLSConfig(cr *certRouter, l *resolved.Listener) *tls.Config {
	cfg := &tls.Config{
		GetCertificate: cr.GetCertificate,
		MinVersion:     tls.VersionTLS12,
	}
	protos := []string{alpnHTTP1}
	if l.EnableHTTP2 {
		protos = append([]string{alpnHTTP2}, protos...)
	}
	if cr.hasACMETLS && !l.BehindCloudflare {
		protos = append(protos, alpnACMETLS)
	}
	cfg.NextProtos = protos
	return cfg
}
