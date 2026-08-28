package statute

import (
	"fmt"
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
)

// ClientAuth configures client-certificate authentication for one HTTPS
// listener. The policy covers every TLS source and both TCP and HTTP/3.
// Verifying modes require at least one CA file.
type ClientAuth struct {
	Mode    ClientAuthMode
	CAFiles []string
}

func (p ClientAuth) applyListener(l *Listener) {
	l.clientAuth = append(l.clientAuth, p)
}

func resolveClientAuth(l *Listener, rl *resolved.Listener) error {
	if len(l.clientAuth) == 0 {
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

	p := l.clientAuth[0]
	mode, ok := resolvedClientAuthMode(p.Mode)
	if !ok {
		return fmt.Errorf("client_auth: unsupported mode %d", p.Mode)
	}
	for i, path := range p.CAFiles {
		if strings.TrimSpace(path) == "" {
			return fmt.Errorf("client_auth: ca_files[%d]: path is empty", i)
		}
	}
	if clientAuthVerifies(p.Mode) && len(p.CAFiles) == 0 {
		return fmt.Errorf("client_auth: mode %s requires at least one CA file", mode)
	}
	rl.ClientAuth = &resolved.ClientAuth{
		Mode:    mode,
		CAFiles: append([]string(nil), p.CAFiles...),
	}
	return nil
}

func resolvedClientAuthMode(mode ClientAuthMode) (resolved.ClientAuthMode, bool) {
	switch mode {
	case RequestClientCert:
		return resolved.ClientAuthRequest, true
	case RequireAnyClientCert:
		return resolved.ClientAuthRequireAny, true
	case VerifyClientCertIfGiven:
		return resolved.ClientAuthVerifyIfGiven, true
	case RequireAndVerifyClientCert:
		return resolved.ClientAuthRequireAndVerify, true
	default:
		return "", false
	}
}

func clientAuthVerifies(mode ClientAuthMode) bool {
	return mode == VerifyClientCertIfGiven || mode == RequireAndVerifyClientCert
}

func clientAuthRequiresCertificate(policy *resolved.ClientAuth) bool {
	if policy == nil {
		return false
	}
	return policy.Mode != resolved.ClientAuthRequest && policy.Mode != resolved.ClientAuthVerifyIfGiven
}
