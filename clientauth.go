package statute

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"

	"statute.kjanat.dev/resolved"
)

// ClientAuthMode controls how an HTTPS listener handles client certificates.
type ClientAuthMode uint8

// Supported client-certificate handshake policies.
const (
	RequestClientCert ClientAuthMode = iota + 1
	RequireAnyClientCert
	VerifyClientCertIfGiven
	RequireAndVerifyClientCert
	// NoClientCert clears a listener default for selected SNI names.
	NoClientCert
)

// ClientAuth configures client-certificate authentication for one HTTPS
// listener by default. ClientAuthFor overrides it for selected SNI names.
// The policy covers every TLS source and both TCP and HTTP/3.
// Verifying modes require at least one CA file.
type ClientAuth struct {
	Mode    ClientAuthMode
	CAFiles []string
}

func (p ClientAuth) applyListener(l *Listener) {
	l.clientAuth = append(l.clientAuth, p)
}

type clientAuthPolicy struct {
	hosts  []string
	policy ClientAuth
}

// ClientAuthFor selects a client-certificate policy for the given SNI names.
// Names use TLS source canonicalization and exact-before-single-label-wildcard
// precedence. Unmatched names and absent SNI use the listener-wide ClientAuth.
// The policy is independent of certificate selection, including StaticTLS fallback.
// HTTP requests whose authority selects a different effective policy receive 421.
// Policies are equivalent when their mode and loaded CA certificate DER sets
// match, regardless of declaration grouping, file paths, ordering or duplicates.
func ClientAuthFor(hosts []string, policy ClientAuth) ListenerOption {
	return clientAuthPolicy{hosts: append([]string(nil), hosts...), policy: cloneClientAuth(policy)}
}

func cloneClientAuth(p ClientAuth) ClientAuth {
	p.CAFiles = append([]string(nil), p.CAFiles...)
	return p
}

func (p clientAuthPolicy) applyListener(l *Listener) {
	l.clientAuthPolicies = append(l.clientAuthPolicies, clientAuthPolicy{
		hosts: append([]string(nil), p.hosts...), policy: cloneClientAuth(p.policy),
	})
}

func resolveClientAuth(l *Listener, rl *resolved.Listener) error {
	if len(l.clientAuth) == 0 && len(l.clientAuthPolicies) == 0 {
		return nil
	}
	if len(l.clientAuth) > 1 {
		return fmt.Errorf("client_auth: one policy allowed per listener, got %d", len(l.clientAuth))
	}
	if l.scheme != schemeHTTPS {
		return fmt.Errorf("client_auth: requires an HTTPS listener")
	}
	if l.redirect != "" {
		return fmt.Errorf("client_auth: redirect-only listener does not terminate TLS")
	}

	if len(l.clientAuth) != 0 {
		policy, err := resolveClientAuthPolicy(l.clientAuth[0])
		if err != nil {
			return err
		}
		rl.ClientAuth = policy
	}
	claimed := make(map[string]bool)
	for i, p := range l.clientAuthPolicies {
		binding, err := resolveScopedClientAuth(p, claimed)
		if err != nil {
			return fmt.Errorf("client_auth_policies[%d]: %w", i, err)
		}
		rl.ClientAuthPolicies = append(rl.ClientAuthPolicies, binding)
	}
	return nil
}

func resolveScopedClientAuth(p clientAuthPolicy, claimed map[string]bool) (*resolved.ClientAuthPolicy, error) {
	if len(p.hosts) == 0 {
		return nil, fmt.Errorf("hosts: at least one hostname required")
	}
	hosts := make([]string, len(p.hosts))
	for i, name := range p.hosts {
		host := canonicalTLSName(name)
		if host == "" {
			return nil, fmt.Errorf("hosts[%d]: hostname is empty", i)
		}
		if strings.ContainsAny(host, ":/\\ \t\r\n@?#[]%") {
			return nil, fmt.Errorf("hosts[%d]: invalid SNI hostname %q", i, name)
		}
		if err := validateTLSWildcard(host); err != nil {
			return nil, fmt.Errorf("hosts[%d]: %w", i, err)
		}
		if claimed[host] {
			return nil, fmt.Errorf("hostname %q is claimed by two client-auth bindings", host)
		}
		claimed[host] = true
		hosts[i] = host
	}
	policy, err := resolveClientAuthPolicy(p.policy)
	if err != nil {
		return nil, err
	}
	return &resolved.ClientAuthPolicy{Hosts: hosts, Policy: *policy}, nil
}

func resolveClientAuthPolicy(p ClientAuth) (*resolved.ClientAuth, error) {
	mode, err := resolvedClientAuthMode(p.Mode)
	if err != nil {
		return nil, err
	}
	p.CAFiles = append([]string(nil), p.CAFiles...)
	for i := range p.CAFiles {
		p.CAFiles[i] = strings.TrimSpace(p.CAFiles[i])
		if p.CAFiles[i] == "" {
			return nil, fmt.Errorf("client_auth: ca_files[%d]: path is empty", i)
		}
	}
	if clientAuthVerifies(p.Mode) && len(p.CAFiles) == 0 {
		return nil, fmt.Errorf("client_auth: mode %s requires at least one CA file", mode)
	}
	return &resolved.ClientAuth{
		Mode:    mode,
		CAFiles: p.CAFiles,
	}, nil
}

// indexClientAuthPolicies is the offline selector for ACME validation. Runtime
// construction compiles its own immutable table with loaded material and IDs.
func indexClientAuthPolicies(l *resolved.Listener) (tlsNameIndex[*resolved.ClientAuth], error) {
	var index tlsNameIndex[*resolved.ClientAuth]
	for _, binding := range l.ClientAuthPolicies {
		for _, pattern := range binding.Hosts {
			if err := index.add(pattern, &binding.Policy); err != nil {
				return index, err
			}
		}
	}
	return index, nil
}

func clientAuthAuthority(name string) (string, bool) {
	if strings.HasPrefix(name, "[") && strings.HasSuffix(name, "]") {
		ip := strings.TrimSuffix(strings.TrimPrefix(name, "["), "]")
		return ip, net.ParseIP(ip) != nil
	}
	if strings.Contains(name, ":") {
		host, port, err := net.SplitHostPort(name)
		if err != nil || !validAuthorityPort(port) {
			return "", false
		}
		if strings.Contains(host, ":") {
			return host, net.ParseIP(host) != nil
		}
		name = host
	}
	if name == "" || strings.ContainsAny(name, " /\\\t\r\n@?#[]%*") {
		return "", false
	}
	host := canonicalTLSName(name)
	return host, host != ""
}

func validAuthorityPort(port string) bool {
	for _, c := range port {
		if c < '0' || c > '9' {
			return false
		}
	}
	_, err := strconv.ParseUint(port, 10, 16)
	return err == nil
}

func clientAuthAdmission(table *clientAuthTable, next http.Handler) http.Handler {
	if table == nil || !table.scoped {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, valid := clientAuthAuthority(r.Host)
		if !valid || r.TLS == nil || table.selectPolicy(host).id != table.selectPolicy(r.TLS.ServerName).id {
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "client authentication policy differs from TLS SNI", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func resolvedClientAuthMode(mode ClientAuthMode) (resolved.ClientAuthMode, error) {
	if mode == 0 {
		return "", fmt.Errorf("client_auth: mode is required")
	}
	switch mode {
	case NoClientCert:
		return resolved.ClientAuthNone, nil
	case RequestClientCert:
		return resolved.ClientAuthRequest, nil
	case RequireAnyClientCert:
		return resolved.ClientAuthRequireAny, nil
	case VerifyClientCertIfGiven:
		return resolved.ClientAuthVerifyIfGiven, nil
	case RequireAndVerifyClientCert:
		return resolved.ClientAuthRequireAndVerify, nil
	default:
		return "", fmt.Errorf("client_auth: unsupported mode %d", mode)
	}
}

func clientAuthVerifies(mode ClientAuthMode) bool {
	return mode == VerifyClientCertIfGiven || mode == RequireAndVerifyClientCert
}

func clientAuthRequiresCertificate(policy *resolved.ClientAuth) bool {
	if policy == nil {
		return false
	}
	return policy.Mode == resolved.ClientAuthRequireAny || policy.Mode == resolved.ClientAuthRequireAndVerify
}
