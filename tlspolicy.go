package statute

import (
	"crypto/tls"
	"errors"
	"fmt"
	"slices"
	"strings"

	"statute.kjanat.dev/resolved"
)

// TLSVersion names a downstream TLS protocol version. The zero value means
// "unset" and leaves that bound at statute's own default.
//
// TLS12 and TLS13 are the only values Resolve accepts. There are
// deliberately no constants for TLS 1.0 or 1.1 — the library's floor is
// TLS 1.2 and no option lowers it — and a hand-converted
// TLSVersion(tls.VersionTLS10) is a resolve error, not a lower floor.
type TLSVersion uint16

// Downstream TLS protocol versions a TLSPolicy may name.
const (
	// TLS12 is TLS 1.2, the lowest version statute negotiates.
	TLS12 TLSVersion = tls.VersionTLS12
	// TLS13 is TLS 1.3.
	TLS13 TLSVersion = tls.VersionTLS13
)

// CipherSuite names one TLS 1.2 cipher suite a listener may permit. TLS 1.3
// suites are absent by design — Go does not allow configuring them, so
// there would be nothing for a value to change.
//
// Every suite here is ECDHE (forward secret). The static-RSA key exchange
// and 3DES suites crypto/tls still recognises deliberately have no
// constants, and a hand-converted CipherSuite value for one is a resolve
// error.
type CipherSuite uint16

// TLS 1.2 cipher suites a TLSPolicy may permit.
//
// Any non-empty list must include TLSECDHEECDSAWithAES128GCM or
// TLSECDHERSAWithAES128GCM: net/http checks every TLS 1.0–1.2 suite
// override for one of those two before it will serve TLS at all — with
// HTTP/2 enabled or not — so a list without them is a resolve error here
// rather than a listener that binds and never answers a handshake. The
// CBC suites exist for legacy TLS 1.2 clients that support nothing newer;
// they ride alongside the required AES-128-GCM suite, and HTTP/2 never
// negotiates them (RFC 9113 §9.2.2 blacklists CBC).
const (
	// TLSECDHEECDSAWithAES128GCM is ECDHE-ECDSA-AES128-GCM-SHA256 (AEAD, h2-compatible).
	TLSECDHEECDSAWithAES128GCM CipherSuite = CipherSuite(tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256)
	// TLSECDHERSAWithAES128GCM is ECDHE-RSA-AES128-GCM-SHA256 (AEAD, h2-compatible).
	TLSECDHERSAWithAES128GCM CipherSuite = CipherSuite(tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256)
	// TLSECDHEECDSAWithAES256GCM is ECDHE-ECDSA-AES256-GCM-SHA384 (AEAD, h2-compatible).
	TLSECDHEECDSAWithAES256GCM CipherSuite = CipherSuite(tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384)
	// TLSECDHERSAWithAES256GCM is ECDHE-RSA-AES256-GCM-SHA384 (AEAD, h2-compatible).
	TLSECDHERSAWithAES256GCM CipherSuite = CipherSuite(tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384)
	// TLSECDHEECDSAWithChaCha20 is ECDHE-ECDSA-CHACHA20-POLY1305 (AEAD, h2-compatible).
	TLSECDHEECDSAWithChaCha20 CipherSuite = CipherSuite(tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256)
	// TLSECDHERSAWithChaCha20 is ECDHE-RSA-CHACHA20-POLY1305 (AEAD, h2-compatible).
	TLSECDHERSAWithChaCha20 CipherSuite = CipherSuite(tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256)
	// TLSECDHEECDSAWithAES128CBC is ECDHE-ECDSA-AES128-SHA (CBC; never used by HTTP/2).
	TLSECDHEECDSAWithAES128CBC CipherSuite = CipherSuite(tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA)
	// TLSECDHERSAWithAES128CBC is ECDHE-RSA-AES128-SHA (CBC; never used by HTTP/2).
	TLSECDHERSAWithAES128CBC CipherSuite = CipherSuite(tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA)
	// TLSECDHEECDSAWithAES256CBC is ECDHE-ECDSA-AES256-SHA (CBC; never used by HTTP/2).
	TLSECDHEECDSAWithAES256CBC CipherSuite = CipherSuite(tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA)
	// TLSECDHERSAWithAES256CBC is ECDHE-RSA-AES256-SHA (CBC; never used by HTTP/2).
	TLSECDHERSAWithAES256CBC CipherSuite = CipherSuite(tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA)
)

// TLSPolicy configures the downstream TLS protocol policy of one HTTPS
// listener: the protocol version window and the TLS 1.2 cipher suites the
// server is willing to negotiate. It is a ListenerOption, so it goes in the
// HTTPS argument list alongside the TLS sources:
//
//	statute.HTTPS(":443",
//	    statute.AutoTLS("foo.example.test").
//	        Email("ops@example.test").Storage("/var/lib/statute/acme"),
//	    statute.TLSPolicy{
//	        MinVersion: statute.TLS12,
//	        MaxVersion: statute.TLS13,
//	        CipherSuites: []statute.CipherSuite{
//	            statute.TLSECDHEECDSAWithAES128GCM,
//	            statute.TLSECDHERSAWithAES128GCM,
//	        },
//	    },
//	)
//
// The zero policy is what a listener without one gets: minimum TLS 1.2, no
// upper bound, and Go's own TLS 1.2 suite selection. Declaring the policy
// twice on one listener is a resolve error rather than a silent last-wins.
//
// One policy governs the whole listener — every TLS source on it, and the
// HTTP/3 listener that shares its certificates.
type TLSPolicy struct {
	// MinVersion is the lowest version the listener will negotiate. Zero
	// leaves the default of TLS 1.2.
	MinVersion TLSVersion
	// MaxVersion is the highest version the listener will negotiate. Zero
	// leaves it uncapped, so TLS 1.3 stays available.
	MaxVersion TLSVersion
	// CipherSuites restricts TLS 1.2 handshakes to the listed suites.
	// Empty keeps Go's own selection. Declaration order is preserved in
	// the resolved schema, but crypto/tls ranks the listed suites itself —
	// the order here is not a preference order.
	//
	// A non-empty list must include TLSECDHEECDSAWithAES128GCM or
	// TLSECDHERSAWithAES128GCM; net/http refuses to serve TLS without one
	// of them (see the constant block above), so resolve rejects the list
	// early instead.
	//
	// This governs TLS 1.2 only: TLS 1.3 suites are fixed by the protocol
	// and crypto/tls does not accept overrides for them. Pinning suites on
	// a listener whose minimum is already TLS 1.3 is therefore a resolve
	// error, not a silently dead setting.
	CipherSuites []CipherSuite
}

func (p TLSPolicy) applyListener(l *Listener) { l.tlsPolicy = append(l.tlsPolicy, p) }

// tls12CipherSuites lists every suite a TLSPolicy may name, in constant
// declaration order. It is the one source of truth for both directions of
// the name mapping: resolve normalises a suite to its IANA name through
// tls.CipherSuiteName, and applyTLSPolicy maps that name back through the
// table built below, so the two can never drift apart.
var tls12CipherSuites = []CipherSuite{
	TLSECDHEECDSAWithAES128GCM,
	TLSECDHERSAWithAES128GCM,
	TLSECDHEECDSAWithAES256GCM,
	TLSECDHERSAWithAES256GCM,
	TLSECDHEECDSAWithChaCha20,
	TLSECDHERSAWithChaCha20,
	TLSECDHEECDSAWithAES128CBC,
	TLSECDHERSAWithAES128CBC,
	TLSECDHEECDSAWithAES256CBC,
	TLSECDHERSAWithAES256CBC,
}

// tls12ECDSACipherSuites is the ECDHE_ECDSA subset of tls12CipherSuites —
// the suites an ECDSA certificate can serve under TLS 1.2. The in-tree
// ACME manager behind pinned HTTP-01/DNS-01 sources generates ECDSA P-256
// keys without exception, so for a pinned source's domains these are the
// only suites that can ever complete a 1.2 handshake. autocert
// (ChallengeAuto) is different: it picks each leaf's key type from the
// ClientHello — ECDSA P-256 unless the client advertises no ECDSA support,
// RSA-2048 then.
var tls12ECDSACipherSuites = []CipherSuite{
	TLSECDHEECDSAWithAES128GCM,
	TLSECDHEECDSAWithAES256GCM,
	TLSECDHEECDSAWithChaCha20,
	TLSECDHEECDSAWithAES128CBC,
	TLSECDHEECDSAWithAES256CBC,
}

// cipherSuiteIDByName inverts the normalisation resolve applies, mapping an
// IANA suite name in the resolved schema back to its crypto/tls ID.
var cipherSuiteIDByName = func() map[string]uint16 {
	m := make(map[string]uint16, len(tls12CipherSuites))
	for _, cs := range tls12CipherSuites {
		m[tls.CipherSuiteName(uint16(cs))] = uint16(cs)
	}
	return m
}()

// Resolved-schema names for the two versions a policy can express.
const (
	tlsVersionName12 = "1.2"
	tlsVersionName13 = "1.3"
)

// tlsVersionName normalises a surface version to its resolved-schema form.
// The zero value normalises to the empty string, meaning "unset".
func tlsVersionName(v TLSVersion) string {
	switch v {
	case TLS12:
		return tlsVersionName12
	case TLS13:
		return tlsVersionName13
	default:
		return ""
	}
}

// tlsVersionID maps a resolved-schema version name back to its crypto/tls
// constant. The second result is false for the empty string and for any
// name the schema does not define, so callers leave the field alone.
func tlsVersionID(name string) (uint16, bool) {
	switch name {
	case tlsVersionName12:
		return tls.VersionTLS12, true
	case tlsVersionName13:
		return tls.VersionTLS13, true
	default:
		return 0, false
	}
}

// resolveTLSPolicy validates the listener's downstream TLS policy and
// lowers it into the resolved schema. A listener without one keeps
// rl.TLSPolicy nil, which the runtime reads as "statute's defaults".
func resolveTLSPolicy(l *Listener, rl *resolved.Listener) error {
	if len(l.tlsPolicy) == 0 {
		return nil
	}
	if l.scheme != schemeHTTPS {
		return errors.New("tls_policy: only an HTTPS listener has a downstream TLS policy")
	}
	if l.redirect != "" {
		return errors.New("tls_policy: a redirect-only listener terminates no TLS, so the policy would govern nothing; drop the policy or the redirect")
	}
	if len(l.tlsPolicy) > 1 {
		return fmt.Errorf("tls_policy: declared %d times on listener %s; a listener has one downstream TLS policy", len(l.tlsPolicy), l.addr)
	}
	p := l.tlsPolicy[0]
	if err := validateTLSPolicyVersions(p); err != nil {
		return err
	}
	suites, err := resolveTLSPolicySuites(p)
	if err != nil {
		return err
	}
	if err := validateTLSPolicyProtocols(p, rl); err != nil {
		return err
	}
	rl.TLSPolicy = &resolved.TLSPolicy{
		MinVersion:   tlsVersionName(p.MinVersion),
		MaxVersion:   tlsVersionName(p.MaxVersion),
		CipherSuites: suites,
	}
	return nil
}

// validateTLSPolicyVersions rejects versions outside the supported set and
// an inverted window.
func validateTLSPolicyVersions(p TLSPolicy) error {
	if err := validateTLSPolicyVersion("min_version", p.MinVersion); err != nil {
		return err
	}
	if err := validateTLSPolicyVersion("max_version", p.MaxVersion); err != nil {
		return err
	}
	if p.MinVersion != 0 && p.MaxVersion != 0 && p.MinVersion > p.MaxVersion {
		return fmt.Errorf("tls_policy: min_version %s is above max_version %s; the version window is empty", tlsVersionName(p.MinVersion), tlsVersionName(p.MaxVersion))
	}
	return nil
}

// validateTLSPolicyVersion rejects one bound that is neither unset nor one
// of the two versions statute negotiates.
func validateTLSPolicyVersion(field string, v TLSVersion) error {
	if v != 0 && v != TLS12 && v != TLS13 {
		return fmt.Errorf("tls_policy: %s %#04x is not a supported version; use statute.TLS12 or statute.TLS13 (or leave it unset)", field, uint16(v))
	}
	return nil
}

// resolveTLSPolicySuites normalises the permitted TLS 1.2 suites to their
// IANA names, preserving declaration order, and rejects unknown entries,
// repeats, and an override that TLS 1.3 would make dead.
func resolveTLSPolicySuites(p TLSPolicy) ([]string, error) {
	if len(p.CipherSuites) == 0 {
		return nil, nil
	}
	if p.MinVersion == TLS13 {
		return nil, errors.New("tls_policy: cipher_suites governs TLS 1.2 handshakes only, and min_version 1.3 rules those out; drop one of the two")
	}
	out := make([]string, 0, len(p.CipherSuites))
	for _, cs := range p.CipherSuites {
		if !slices.Contains(tls12CipherSuites, cs) {
			return nil, fmt.Errorf("tls_policy: cipher suite %#04x is not one of the suites statute exposes; use the statute.TLSECDHE* constants", uint16(cs))
		}
		name := tls.CipherSuiteName(uint16(cs))
		if slices.Contains(out, name) {
			return nil, fmt.Errorf("tls_policy: cipher suite %s is listed twice", name)
		}
		out = append(out, name)
	}
	return out, nil
}

// validateTLSPolicyProtocols rejects a policy the listener's own transport
// stack could never serve: a version window HTTP/3 cannot work under, a
// suite list net/http refuses to start with, and a suite list the
// listener's certificates could never sign for.
func validateTLSPolicyProtocols(p TLSPolicy, rl *resolved.Listener) error {
	// QUIC is defined over TLS 1.3 alone (RFC 9001 §4.2), so a 1.2 cap
	// would leave the HTTP/3 listener unable to complete any handshake.
	if rl.HTTP3Addr != "" && p.MaxVersion == TLS12 {
		return errors.New("tls_policy: max_version 1.2 cannot serve HTTP/3; QUIC requires TLS 1.3, so raise the cap or drop HTTP3()")
	}
	if err := validateTLSPolicyRequiredSuite(p); err != nil {
		return err
	}
	return validateTLSPolicyCertCompat(p, rl)
}

// validateTLSPolicyRequiredSuite enforces net/http's own precondition: on
// every ServeTLS call — whether or not the listener advertises h2 — its
// HTTP/2 support inspects a TLS 1.0–1.2 cipher-suite override and refuses
// to serve without an AES-128-GCM suite in it. That refusal would surface
// as a listener that binds and then never answers a handshake, so reject
// the configuration here instead.
func validateTLSPolicyRequiredSuite(p TLSPolicy) error {
	if len(p.CipherSuites) == 0 ||
		slices.Contains(p.CipherSuites, TLSECDHEECDSAWithAES128GCM) ||
		slices.Contains(p.CipherSuites, TLSECDHERSAWithAES128GCM) {
		return nil
	}
	return errors.New("tls_policy: cipher_suites must include statute.TLSECDHEECDSAWithAES128GCM or statute.TLSECDHERSAWithAES128GCM; net/http refuses to serve TLS with a suite override that omits both, even with HTTP/2 disabled")
}

// validateTLSPolicyCertCompat rejects an RSA-only suite list that would
// leave a pinned source's domains unservable. The judgement is per source,
// not per listener: the SNI router picks the source that matches the name
// and never falls back past it, so no other certificate on the listener —
// static fallback included — can rescue a source whose own certificate the
// policy cannot authenticate. The in-tree ACME manager behind every
// pinned HTTP-01/DNS-01 source generates ECDSA P-256 keys without
// exception, and an ECDHE_RSA suite needs an RSA certificate to sign the
// key exchange, so with TLS 1.3 capped away those domains are provably
// dead.
//
// ChallengeAuto sources are lint rule TLS004's territory instead of a
// resolve error: autocert picks each leaf's key type from the ClientHello,
// so their fate depends on the client population, not the config alone.
// Static sources are never judged — resolve does not read the files, and
// an RSA key pair serves an RSA-only policy fine.
func validateTLSPolicyCertCompat(p TLSPolicy, rl *resolved.Listener) error {
	if p.MaxVersion != TLS12 || len(p.CipherSuites) == 0 || tlsPolicyHasECDSASuite(p.CipherSuites) {
		return nil
	}
	for _, a := range rl.AutoTLSSources {
		if a.Challenge == resolved.ChallengeAuto {
			continue
		}
		return fmt.Errorf("tls_policy: %s is pinned to %s and the in-tree ACME manager always generates ECDSA P-256 certificate keys, but max_version 1.2 with an RSA-only cipher_suites list cannot authenticate an ECDSA certificate, and the SNI router never falls back past the source that matched the name; add an ECDHE-ECDSA suite or lift max_version", strings.Join(a.Domains, ", "), acmeChallengeLabel(a))
	}
	return nil
}

// tlsPolicyHasECDSASuite reports whether the list holds at least one suite
// an ECDSA certificate can serve.
func tlsPolicyHasECDSASuite(suites []CipherSuite) bool {
	for _, cs := range suites {
		if slices.Contains(tls12ECDSACipherSuites, cs) {
			return true
		}
	}
	return false
}

// acmeChallengeLabel names a pinned source's challenge for error messages.
func acmeChallengeLabel(a *resolved.AutoTLS) string {
	if a.Challenge == resolved.ChallengeDNS01 {
		return "DNS-01"
	}
	return "HTTP-01"
}
