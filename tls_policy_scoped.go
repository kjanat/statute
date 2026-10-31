package statute

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"statute.kjanat.dev/resolved"
)

type hostTLSPolicy struct {
	hosts  []string
	policy TLSPolicy
}

// TLSPolicyFor selects protocol, cipher and strict-SNI policy by SNI hostname.
// ClientAuthFor independently selects the client-certificate requirement.
func TLSPolicyFor(hosts []string, policy TLSPolicy) ListenerOption {
	policy.CipherSuites = append([]CipherSuite(nil), policy.CipherSuites...)
	return hostTLSPolicy{hosts: append([]string(nil), hosts...), policy: policy}
}
func (p hostTLSPolicy) applyListener(l *Listener) { l.tlsPolicies = append(l.tlsPolicies, p) }

func resolveScopedTLSPolicies(l *Listener, out *resolved.Listener) error {
	if len(l.tlsPolicies) == 0 {
		return validateTLSPolicyCertCompat(l, out)
	}
	if l.scheme != schemeHTTPS || l.redirect != "" {
		return errors.New("tls_policies: requires an HTTPS listener")
	}
	claimed := map[string]bool{}
	for _, binding := range l.tlsPolicies {
		hosts, err := resolveTLSPolicyHosts(binding.hosts, claimed)
		if err != nil {
			return err
		}
		suites, err := resolveScopedPolicySuites(binding.policy)
		if err != nil {
			return err
		}
		if out.HTTP3Addr != "" && binding.policy.MaxVersion == TLS12 {
			return errors.New("tls_policies: TLS 1.2 maximum cannot serve HTTP/3")
		}
		out.TLSPolicies = append(out.TLSPolicies, resolved.HostTLSPolicy{Hosts: hosts, Policy: resolved.TLSPolicy{StrictSNI: binding.policy.StrictSNI, Identity: binding.policy.Identity, MinVersion: tlsVersionName(binding.policy.MinVersion), MaxVersion: tlsVersionName(binding.policy.MaxVersion), CipherSuites: suites}})
	}
	return validateTLSPolicyCertCompat(l, out)
}

type tlsPolicyTable struct {
	names    tlsNameIndex[*resolved.TLSPolicy]
	fallback *resolved.TLSPolicy
}

func buildTLSPolicyTable(l *resolved.Listener) (*tlsPolicyTable, error) {
	table := &tlsPolicyTable{fallback: l.TLSPolicy}
	for i := range l.TLSPolicies {
		binding := &l.TLSPolicies[i]
		for _, host := range binding.Hosts {
			if err := table.names.add(host, &binding.Policy); err != nil {
				return nil, err
			}
		}
	}
	return table, nil
}

func (t *tlsPolicyTable) selectPolicy(host string) *resolved.TLSPolicy {
	if t == nil {
		return nil
	}
	if policy, ok := t.names.lookup(host); ok {
		return policy
	}
	return t.fallback
}

func tlsPolicyAdmission(table *tlsPolicyTable, next http.Handler) http.Handler {
	if table == nil || len(table.names.exact)+len(table.names.wildcards) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authority, valid := clientAuthAuthority(r.Host)
		if r.TLS != nil && (!valid || !reflect.DeepEqual(table.selectPolicy(r.TLS.ServerName), table.selectPolicy(authority))) {
			http.Error(w, http.StatusText(http.StatusMisdirectedRequest), http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func strictSNICertificate(router *certRouter, table *tlsPolicyTable, hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	policy := table.selectPolicy(hello.ServerName)
	if policy != nil && policy.StrictSNI && hello.ServerName == "" {
		return nil, errors.New("tls: SNI is required")
	}
	certificate, err := router.GetCertificate(hello)
	if err != nil {
		return nil, err
	}
	if policy != nil && policy.StrictSNI {
		leaf := certLeaf(certificate)
		if leaf == nil || leaf.VerifyHostname(canonicalTLSName(hello.ServerName)) != nil {
			return nil, errors.New("tls: certificate does not cover SNI")
		}
	}
	return certificate, nil
}

func resolveTLSPolicyHosts(names []string, claimed map[string]bool) ([]string, error) {
	if len(names) == 0 {
		return nil, errors.New("tls_policies: at least one hostname required")
	}
	hosts := make([]string, len(names))
	for i, name := range names {
		host := canonicalTLSName(name)
		if host == "" || strings.ContainsAny(host, ":/\\ \t\r\n@?#[]%") {
			return nil, errors.New("tls_policies: invalid hostname")
		}
		if err := validateTLSWildcard(host); err != nil {
			return nil, err
		}
		if claimed[host] {
			return nil, fmt.Errorf("tls_policies: hostname %q is declared twice", host)
		}
		claimed[host] = true
		hosts[i] = host
	}
	return hosts, nil
}

func resolveScopedPolicySuites(policy TLSPolicy) ([]string, error) {
	if err := validateTLSPolicyVersions(policy); err != nil {
		return nil, err
	}
	suites, err := resolveTLSPolicySuites(policy)
	if err != nil {
		return nil, err
	}
	if err = validateTLSPolicyRequiredSuite(policy); err != nil {
		return nil, err
	}
	return suites, nil
}
