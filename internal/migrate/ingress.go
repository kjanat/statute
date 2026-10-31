package migrate

import (
	"fmt"
	"net/netip"
	"strconv"

	statute "statute.kjanat.dev"
)

func (g *generator) listenerIngress(o *Object, l *statute.Listener, code string) (*statute.Listener, string) {
	p := statute.IngressPolicy{ForwardedHeaders: true, NormalizePath: true, SanitizePath: true, HeaderAliases: statute.HeaderAliasesKeep}
	if value := o.value("http.sanitizePath"); value != nil || hasFieldPrefix(o, "http.sanitizePath") {
		if b, ok := value.(bool); ok {
			p.SanitizePath = b
			o.mark("http.sanitizePath", Translated, "explicit pre-routing path sanitation")
		} else {
			o.mark("http.sanitizePath", Manual, "path sanitation requires a literal boolean")
		}
	}
	p.HeaderAliases = ingressAliasPolicy(o)
	l.With(p)
	code += fmt.Sprintf(".With(statute.IngressPolicy{ForwardedHeaders:true,NormalizePath:true,SanitizePath:%t,HeaderAliases:%s})", p.SanitizePath, headerAliasCode(p.HeaderAliases))
	if cidrs, ok := ingressProxyRanges(o); ok && len(cidrs) > 0 {
		l.With(statute.TrustedProxy(cidrs...))
		code += ".With(statute.TrustedProxy(" + quoted(cidrs) + "))"
	}
	classifyIngressForwarding(o)
	return l, code
}

func classifyIngressForwarding(o *Object) {
	if value, ok := o.value("forwardedHeaders").(map[string]any); ok && len(value) == 0 {
		o.mark("forwardedHeaders", Translated, "default direct-peer forwarded-header policy")
	}
	if value := o.value("forwardedHeaders.connection"); value != nil {
		if emptyHeaderPolicy(value) {
			o.mark("forwardedHeaders.connection", Exact, "default Connection-header sanitation")
		} else {
			o.mark("forwardedHeaders.connection", Manual, "connection-nominated headers are stripped before middleware; an allow-list needs explicit native policy")
		}
	}
	if value := o.value("forwardedHeaders.notAppendXForwardedFor"); value != nil {
		if scalar(value) == falseValue {
			o.mark("forwardedHeaders.notAppendXForwardedFor", Exact, "append the direct peer to the outbound forwarded chain")
		} else {
			o.mark("forwardedHeaders.notAppendXForwardedFor", Manual, "native proxy appends the direct peer; disabling that append requires explicit policy")
		}
	}
}

func ingressAliasPolicy(o *Object) statute.HeaderAliasPolicy {
	path := "http.aliasHeadersStrategy"
	value := o.text(path)
	legacy := value == ""
	if legacy {
		path = "http.underscoreHeadersStrategy"
		value = o.text(path)
	}
	if value == "" {
		return statute.HeaderAliasesKeep
	}
	p := statute.HeaderAliasPolicy(value)
	switch p {
	case statute.HeaderAliasesKeep, statute.HeaderAliasesDelete, statute.HeaderAliasesReject:
		o.mark(path, Translated, "explicit request header alias policy")
		if !legacy {
			o.mark("http.underscoreHeadersStrategy", Exact, "superseded by aliasHeadersStrategy")
		}
		if legacy && p == statute.HeaderAliasesDelete {
			return statute.HeaderUnderscoresDelete
		}
		if legacy && p == statute.HeaderAliasesReject {
			return statute.HeaderUnderscoresReject
		}
	default:
		o.mark(path, Manual, "header alias strategy must be keep, delete or reject")
	}
	return p
}

func ingressProxyRanges(o *Object) ([]string, bool) {
	if value := o.value("forwardedHeaders.insecure"); value != nil {
		b, err := strconv.ParseBool(scalar(value))
		if err != nil {
			o.mark("forwardedHeaders.insecure", Manual, "forwarded-header trust requires a literal boolean")
			return nil, false
		}
		o.mark("forwardedHeaders.insecure", Translated, "explicit forwarded-header trust setting")
		if b {
			o.mark("forwardedHeaders.trustedIPs", Exact, "insecure forwarding trusts every direct peer")
			return []string{"0.0.0.0/0", "::/0"}, true
		}
	}
	if o.value("forwardedHeaders.trustedIPs") == nil {
		return nil, true
	}
	values, ok := stringList(o.value("forwardedHeaders.trustedIPs"), false)
	if !ok {
		o.mark("forwardedHeaders.trustedIPs", Manual, "proxy trust requires literal IP addresses or CIDRs")
		return nil, false
	}
	var cidrs []string
	for _, value := range values {
		if addr, err := netip.ParseAddr(value); err == nil {
			value = netip.PrefixFrom(addr.Unmap(), addr.Unmap().BitLen()).String()
		}
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			o.mark("forwardedHeaders.trustedIPs", Manual, "invalid proxy trust address or CIDR")
			return nil, false
		}
		cidrs = append(cidrs, prefix.Masked().String())
	}
	o.mark("forwardedHeaders.trustedIPs", Translated, "listener-owned direct-peer trust ranges")
	return cidrs, true
}

func headerAliasCode(policy statute.HeaderAliasPolicy) string {
	switch policy {
	case statute.HeaderAliasesKeep:
		return "statute.HeaderAliasesKeep"
	case statute.HeaderAliasesDelete:
		return "statute.HeaderAliasesDelete"
	case statute.HeaderAliasesReject:
		return "statute.HeaderAliasesReject"
	case statute.HeaderUnderscoresDelete:
		return "statute.HeaderUnderscoresDelete"
	case statute.HeaderUnderscoresReject:
		return "statute.HeaderUnderscoresReject"
	default:
		return fmt.Sprintf("statute.HeaderAliasPolicy(%q)", policy)
	}
}
