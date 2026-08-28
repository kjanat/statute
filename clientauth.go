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
	mode, err := resolvedClientAuthMode(p.Mode)
	if err != nil {
		return err
	}
	p.CAFiles = append([]string(nil), p.CAFiles...)
	for i := range p.CAFiles {
		p.CAFiles[i] = strings.TrimSpace(p.CAFiles[i])
		if p.CAFiles[i] == "" {
			return fmt.Errorf("client_auth: ca_files[%d]: path is empty", i)
		}
	}
	if clientAuthVerifies(p.Mode) && len(p.CAFiles) == 0 {
		return fmt.Errorf("client_auth: mode %s requires at least one CA file", mode)
	}
	rl.ClientAuth = &resolved.ClientAuth{
		Mode:    mode,
		CAFiles: p.CAFiles,
	}
	return nil
}

func resolvedClientAuthMode(mode ClientAuthMode) (resolved.ClientAuthMode, error) {
	if mode == 0 {
		return "", fmt.Errorf("client_auth: mode is required")
	}
	switch mode {
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
	return policy.Mode != resolved.ClientAuthRequest && policy.Mode != resolved.ClientAuthVerifyIfGiven
}
