package statute

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"slices"

	"statute.kjanat.dev/resolved"
)

// clientAuthPolicyID identifies an effective policy within one compiled listener.
// It never derives from a declaration address or appears in the resolved model.
type clientAuthPolicyID int

type compiledClientAuth struct {
	id        clientAuthPolicyID
	mode      tls.ClientAuthType
	clientCAs *x509.CertPool
	der       []string // sorted unique full certificate DER; construction-only equality
}

// clientAuthTable owns all authentication material independently of certificate
// sources. Handshakes and HTTP admission share this immutable indexed selection.
type clientAuthTable struct {
	names       tlsNameIndex[*compiledClientAuth]
	defaultAuth *compiledClientAuth
	scoped      bool
}

func buildClientAuthTable(l *resolved.Listener) (*clientAuthTable, error) {
	fallback, err := compileClientAuth(l.ClientAuth)
	if err != nil {
		return nil, err
	}
	table := &clientAuthTable{defaultAuth: fallback, scoped: len(l.ClientAuthPolicies) != 0}
	policies := []*compiledClientAuth{fallback}
	for i, assignment := range l.ClientAuthPolicies {
		// Load every declaration before interning: an equivalent sibling must
		// never hide an unreadable or malformed explicitly configured file.
		policy, err := compileClientAuth(&assignment.Policy)
		if err != nil {
			return nil, fmt.Errorf("client_auth_policies[%d]: %w", i, err)
		}
		policy = internClientAuth(&policies, policy)
		for _, host := range assignment.Hosts {
			if err := table.names.add(host, policy); err != nil {
				return nil, fmt.Errorf("client_auth_policies[%d]: %w", i, err)
			}
		}
	}
	return table, nil
}

func internClientAuth(policies *[]*compiledClientAuth, candidate *compiledClientAuth) *compiledClientAuth {
	for _, existing := range *policies {
		if existing.mode == candidate.mode && slices.Equal(existing.der, candidate.der) {
			return existing
		}
	}
	candidate.id = clientAuthPolicyID(len(*policies))
	*policies = append(*policies, candidate)
	return candidate
}

func (table *clientAuthTable) selectPolicy(name string) *compiledClientAuth {
	if policy, found := table.names.lookup(name); found {
		return policy
	}
	return table.defaultAuth
}

func compileClientAuth(policy *resolved.ClientAuth) (*compiledClientAuth, error) {
	compiled := &compiledClientAuth{mode: tls.NoClientCert}
	if policy == nil {
		return compiled, nil
	}
	mode, err := compileClientAuthMode(policy.Mode)
	if err != nil {
		return nil, err
	}
	compiled.mode = mode
	if len(policy.CAFiles) == 0 {
		if compiled.mode == tls.VerifyClientCertIfGiven || compiled.mode == tls.RequireAndVerifyClientCert {
			return nil, fmt.Errorf("client_auth: mode %s requires at least one CA file", policy.Mode)
		}
		return compiled, nil
	}
	compiled.clientCAs = x509.NewCertPool()
	for _, path := range policy.CAFiles {
		certs, err := loadClientCACertificates(path)
		if err != nil {
			return nil, err
		}
		for _, cert := range certs {
			compiled.clientCAs.AddCert(cert)
			compiled.der = append(compiled.der, string(cert.Raw))
		}
	}
	slices.Sort(compiled.der)
	compiled.der = slices.Compact(compiled.der)
	return compiled, nil
}

func compileClientAuthMode(mode resolved.ClientAuthMode) (tls.ClientAuthType, error) {
	switch mode {
	case resolved.ClientAuthNone:
		return tls.NoClientCert, nil
	case resolved.ClientAuthRequest:
		return tls.RequestClientCert, nil
	case resolved.ClientAuthRequireAny:
		return tls.RequireAnyClientCert, nil
	case resolved.ClientAuthVerifyIfGiven:
		return tls.VerifyClientCertIfGiven, nil
	case resolved.ClientAuthRequireAndVerify:
		return tls.RequireAndVerifyClientCert, nil
	default:
		return tls.NoClientCert, fmt.Errorf("client_auth: unsupported mode %q", mode)
	}
}

// Match AppendCertsFromPEM's accepted blocks, but retain their complete DER for
// exact set comparison. Neither CA subjects nor digest equality proves identity.
func loadClientCACertificates(path string) ([]*x509.Certificate, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: operator-configured CA path
	if err != nil {
		return nil, fmt.Errorf("client CA file: %w", err)
	}
	var certs []*x509.Certificate
	for len(data) != 0 {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err == nil {
			certs = append(certs, cert)
		}
	}
	if len(certs) == 0 {
		return nil, fmt.Errorf("client CA file %s: no certificates found", path)
	}
	return certs, nil
}

func (policy *compiledClientAuth) apply(cfg *tls.Config) {
	cfg.ClientAuth = policy.mode
	cfg.ClientCAs = policy.clientCAs
}

func (policy *compiledClientAuth) requiresCertificate() bool {
	return policy.mode == tls.RequireAnyClientCert || policy.mode == tls.RequireAndVerifyClientCert
}
