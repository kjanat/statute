package statute

import (
	"strings"

	"statute.kjanat.dev/resolved"
)

// Pinned sources have known key types. Validation follows the runtime's exact
// and wildcard precedence for both policies and certificate sources. Static
// certificate files stay unread; automatic sources choose keys at handshake.
func validateTLSPolicyCertCompat(l *Listener, out *resolved.Listener) error {
	var policies tlsNameIndex[TLSPolicy]
	for _, binding := range l.tlsPolicies {
		for _, host := range binding.hosts {
			if err := policies.add(host, binding.policy); err != nil {
				return err
			}
		}
	}
	sources, err := certificateSourceIndex(out)
	if err != nil {
		return err
	}
	var fallback TLSPolicy
	if len(l.tlsPolicy) > 0 {
		fallback = l.tlsPolicy[0]
	}
	for _, source := range out.AutoTLSSources {
		if source.Challenge == resolved.ChallengeAuto {
			continue
		}
		if err := validateSourcePolicies(source, fallback, &policies, &sources); err != nil {
			return err
		}
	}
	return nil
}

func certificateSourceIndex(l *resolved.Listener) (tlsNameIndex[*resolved.AutoTLS], error) {
	var index tlsNameIndex[*resolved.AutoTLS]
	for _, source := range l.AutoTLSSources {
		for _, domain := range source.Domains {
			if err := index.add(domain, source); err != nil {
				return index, err
			}
		}
	}
	for _, source := range l.StaticTLSSources {
		if source.Host != "" {
			if err := index.add(source.Host, nil); err != nil {
				return index, err
			}
		}
	}
	return index, nil
}

func validateSourcePolicies(source *resolved.AutoTLS, fallback TLSPolicy, policies *tlsNameIndex[TLSPolicy], sources *tlsNameIndex[*resolved.AutoTLS]) error {
	for _, domain := range source.Domains {
		policy, ok := policies.lookup(domain)
		if !ok {
			policy = fallback
		}
		if err := validateCertificatePolicy(policy, source, domain); err != nil {
			return err
		}
		if !strings.HasPrefix(domain, "*.") {
			continue
		}
		for host, policy := range policies.exact {
			selected, _ := sources.lookup(host)
			if selected == source {
				if err := validateCertificatePolicy(policy, source, host); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
