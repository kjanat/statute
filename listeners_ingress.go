package statute

import (
	"errors"

	"statute.kjanat.dev/resolved"
)

// HeaderAliasPolicy selects treatment of aliased request header names.
type HeaderAliasPolicy = resolved.HeaderAliasPolicy

// Header alias handling modes.
const (
	HeaderAliasesKeep       = resolved.HeaderAliasesKeep
	HeaderAliasesDelete     = resolved.HeaderAliasesDelete
	HeaderAliasesReject     = resolved.HeaderAliasesReject
	HeaderUnderscoresDelete = resolved.HeaderUnderscoresDelete
	HeaderUnderscoresReject = resolved.HeaderUnderscoresReject
)

// IngressPolicy configures pre-routing request processing. ForwardedHeaders
// sanitizes untrusted proxy headers using the listener's TrustedProxy policy.
// NormalizePath canonicalizes escapes; SanitizePath cleans dot and slash segments.
type IngressPolicy struct {
	ForwardedHeaders bool
	NormalizePath    bool
	SanitizePath     bool
	HeaderAliases    HeaderAliasPolicy
}

func (p IngressPolicy) applyListener(l *Listener) { l.ingress = append(l.ingress, p) }

func resolveIngress(l *Listener, out *resolved.Listener) error {
	if len(l.ingress) == 0 {
		return nil
	}
	if len(l.ingress) != 1 {
		return errors.New("declare one ingress policy per listener")
	}
	p := l.ingress[0]
	if p.HeaderAliases == "" {
		p.HeaderAliases = HeaderAliasesKeep
	}
	switch p.HeaderAliases {
	case HeaderAliasesKeep, HeaderAliasesDelete, HeaderAliasesReject, HeaderUnderscoresDelete, HeaderUnderscoresReject:
	default:
		return errors.New("invalid ingress header alias policy")
	}
	out.Ingress = &resolved.IngressPolicy{ForwardedHeaders: p.ForwardedHeaders, NormalizePath: p.NormalizePath, SanitizePath: p.SanitizePath, HeaderAliases: p.HeaderAliases}
	return nil
}
