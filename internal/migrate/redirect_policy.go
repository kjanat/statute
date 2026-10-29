package migrate

import (
	"net/url"
	"regexp/syntax"
	"strconv"
	"strings"

	"statute.kjanat.dev/internal/docker"
)

const (
	redirectFamilyScheme = "redirectScheme"
	redirectFamilyRegex  = "redirectRegex"
	fieldPermanent       = "permanent"
)

func redirectCandidate(m *Object) (string, int, int, string) {
	family := redirectFamilyScheme
	if m.value(redirectFamilyRegex) != nil || hasFieldPrefix(m, "redirectRegex.") {
		family = redirectFamilyRegex
	}
	status, other, ok := sourceRedirectStatuses(m, family)
	if !ok {
		return "", 0, 0, "redirect status or permanence is invalid or unresolved"
	}
	var target, reason string
	if family == redirectFamilyRegex {
		target, reason = regexRedirectTarget(m.text("redirectRegex.regex"), m.text("redirectRegex.replacement"))
	} else {
		target, reason = schemeRedirectTarget(m)
	}
	return target, status, other, reason
}

func schemeRedirectTarget(m *Object) (string, string) {
	scheme := m.text("redirectScheme.scheme")
	if scheme != protocolHTTP && scheme != protocolHTTPS {
		return "", "redirect target scheme must be literal HTTP or HTTPS"
	}
	port := m.text("redirectScheme.port")
	if port != "" && !validPort(port) {
		return "", "redirect target port must be a literal valid port"
	}
	target := scheme + "://{host}"
	standardPort := (scheme == protocolHTTP && port == "80") || (scheme == protocolHTTPS && port == "443")
	if port != "" && !standardPort {
		target += ":" + port
	}
	return target + "{request_uri}", ""
}

func sourceBoolValue(value any, fallback bool) (bool, bool) {
	if value == nil {
		return fallback, true
	}
	b, err := strconv.ParseBool(scalar(value))
	return b, err == nil
}

func sourceRedirectStatuses(m *Object, family string) (int, int, bool) {
	permanent, ok := sourceBoolValue(m.value(family+"."+fieldPermanent), false)
	if !ok {
		return 0, 0, false
	}
	status, other := 302, 307
	if permanent {
		status, other = 301, 308
	}
	if family == redirectFamilyScheme {
		force, valid := sourceBoolValue(m.value(family+".forcePermanentRedirect"), false)
		if !valid {
			return 0, 0, false
		}
		if force {
			return 308, 308, true
		}
	} else if value := m.value(family + ".statusCode"); value != nil {
		fixed, err := integer(value)
		if err != nil || !validSourceRedirectStatus(fixed) {
			return 0, 0, false
		}
		return fixed, fixed, true
	}
	return status, other, true
}

func validSourceRedirectStatus(status int) bool {
	switch status {
	case 301, 302, 303, 307, 308:
		return true
	default:
		return false
	}
}

func regexRedirectTarget(pattern, replacement string) (string, string) {
	if !literal(strings.TrimSuffix(pattern, "$")) {
		return "", "redirect regexp and replacement require literal input"
	}
	withoutSlash, all, reason := redirectCaptureShape(pattern)
	if reason != "" {
		return "", reason
	}
	target, reason := redirectReplacementTarget(replacement, withoutSlash)
	if reason != "" {
		return "", reason
	}
	if all {
		return target, ""
	}
	return target, "regexp compares raw Host case and port and falls through on non-match; native Host routing ignores case and strips ports"
}

func redirectCaptureShape(pattern string) (bool, bool, string) {
	switch strings.TrimSuffix(pattern, "$") {
	case `^https?://[^/]+/(.*)`:
		return true, true, ""
	case `^https?://[^/]+(.*)`:
		return false, true, ""
	}
	prefix, ok := redirectLiteralPrefix(pattern)
	if !ok {
		return false, false, "regexp matching or capture substitution cannot be represented by a native redirect template"
	}
	u, err := url.Parse(prefix)
	if err != nil || !redirectAuthorityPrefix(u) {
		return false, false, "regexp literal prefix changes path or authority matching beyond native redirect templates"
	}
	return u.Path == "/", false, ""
}

func redirectAuthorityPrefix(u *url.URL) bool {
	return u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && (u.Path == "" || u.Path == "/")
}

func redirectReplacementTarget(replacement string, withoutSlash bool) (string, string) {
	prefix, ok := strings.CutSuffix(replacement, "${1}")
	if !ok {
		prefix, ok = strings.CutSuffix(replacement, "$1")
	}
	if !ok {
		return "", "redirect replacement must end in the complete request-URI capture"
	}
	if strings.Contains(prefix, "$") || strings.ContainsAny(prefix, "{}\r\n") {
		return "", "redirect replacement contains additional captures or template syntax"
	}
	if withoutSlash {
		var found bool
		prefix, found = strings.CutSuffix(prefix, "/")
		if !found {
			return "", "capture excludes the leading slash and replacement does not restore it"
		}
	}
	return prefix + "{request_uri}", ""
}

func redirectLiteralPrefix(pattern string) (string, bool) {
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil || re.Op != syntax.OpConcat || len(re.Sub) < 3 {
		return "", false
	}
	parts := re.Sub
	if parts[0].Op != syntax.OpBeginText || parts[1].Op != syntax.OpLiteral {
		return "", false
	}
	if len(parts) == 4 && parts[3].Op == syntax.OpEndText {
		parts = parts[:3]
	}
	if len(parts) != 3 || !redirectRequestURICapture(parts[2]) {
		return "", false
	}
	return string(parts[1].Rune), true
}

func redirectRequestURICapture(re *syntax.Regexp) bool {
	if re.Op != syntax.OpCapture || len(re.Sub) != 1 {
		return false
	}
	star := re.Sub[0]
	if star.Op != syntax.OpStar || len(star.Sub) != 1 {
		return false
	}
	op := star.Sub[0].Op
	return op == syntax.OpAnyChar || op == syntax.OpAnyCharNotNL
}

func (g *generator) checkRedirectEquality(router, middleware *Object, target string) {
	prefix, _, _ := strings.Cut(target, "{request_uri}")
	u, err := url.Parse(prefix)
	if err != nil || u.Host == "" {
		return
	}
	matchers, err := docker.ParseRule(router.text("rule"))
	if err != nil {
		return
	}
	for _, matcher := range matchers {
		if matcher.Host == "" || strings.EqualFold(strings.TrimSuffix(matcher.Host, "."), strings.TrimSuffix(u.Hostname(), ".")) {
			middleware.note("$redirect-equality", Manual, "source continues to the backend when replacement equals the request URL; unconditional native redirects need a distinct target authority")
			return
		}
	}
}

func (g *generator) tryRedirect(o *Object, matchers []docker.Matcher, refs []string) bool {
	if o.Provider != providerFile {
		return false
	}
	target, status, other, ok := g.redirect(o, refs)
	if !ok {
		return false
	}
	g.redirectRoutes(o, matchers, target, status, other)
	return true
}

func (g *generator) classifyRedirectCandidate(router, m *Object, target, reason string) {
	if m.value(redirectFamilyScheme) != nil || hasFieldPrefix(m, "redirectScheme.") {
		for _, key := range []string{"scheme", "port", fieldPermanent, "forcePermanentRedirect"} {
			m.mark("redirectScheme."+key, Translated, "native redirect action with explicit method statuses")
		}
		g.checkRedirectSchemeScope(router, m, m.text("redirectScheme.scheme"))
		return
	}
	for _, key := range []string{"regex", "replacement", fieldPermanent, "statusCode"} {
		m.mark("redirectRegex."+key, Translated, "native redirect template from the raw-URL capture")
	}
	if reason != "" {
		m.note("$redirect-match", Manual, reason)
		return
	}
	g.checkRedirectEquality(router, m, target)
}

func (g *generator) checkRedirectSchemeScope(router, middleware *Object, target string) {
	for _, name := range g.routerEntries[router] {
		ep := g.entrypoints[name]
		if ep == nil {
			continue
		}
		scheme := protocolHTTP
		if g.entryTLS[name] || ep.value("http.tls") != nil {
			scheme = protocolHTTPS
		}
		if scheme == target {
			middleware.note("$redirect-condition", Manual, "source skips redirects when the request already has the target scheme; native redirect routes are unconditional")
		}
		cidrs, ok := ingressProxyRanges(ep)
		if !ok || len(cidrs) > 0 {
			middleware.note("$redirect-trust", Manual, "trusted forwarded schemes can bypass the source redirect; select explicit listener/route policy")
		}
	}
}
