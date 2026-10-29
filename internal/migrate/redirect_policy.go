package migrate

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	statute "statute.kjanat.dev"
)

const (
	redirectFamilyScheme = "redirectScheme"
	redirectFamilyRegex  = "redirectRegex"
	fieldPermanent       = "permanent"
)

var migrationNoop = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })

func (g *generator) redirectMiddleware(o *Object, chain *middleware) {
	family := ""
	for _, name := range []string{redirectFamilyScheme, redirectFamilyRegex} {
		if o.value(name) != nil || hasFieldPrefix(o, name+".") {
			family = name
		}
	}
	if family == "" {
		return
	}
	status, other, ok := sourceRedirectStatuses(o, family)
	if !ok {
		o.mark(family, Manual, "invalid redirect status or permanence")
		return
	}
	var mw statute.Middleware
	var code string
	if family == redirectFamilyScheme {
		scheme, port := o.text(family+".scheme"), o.text(family+".port")
		if scheme != protocolHTTP && scheme != protocolHTTPS {
			o.mark(family, Manual, "redirect scheme must be http or https")
			return
		}
		if port != "" && !validPort(port) {
			o.mark(family, Manual, "invalid redirect port")
			return
		}
		value := statute.RedirectScheme(scheme)
		code = fmt.Sprintf("statute.RedirectScheme(%q)", scheme)
		if port != "" {
			value.Port(port)
			code += fmt.Sprintf(".Port(%q)", port)
		}
		if status == 301 && other == 308 {
			value.Permanent()
			code += ".Permanent()"
		} else if status == other {
			value.Status(status)
			code += fmt.Sprintf(".Status(%d)", status)
		}
		mw = value
	} else {
		pattern, replacement := o.text(family+".regex"), o.text(family+".replacement")
		if strings.ContainsAny(pattern+replacement, "\r\n\x00") || strings.Contains(pattern, "${") || strings.Contains(pattern+replacement, "{{") {
			o.mark(family, Manual, "redirect contains unresolved interpolation or invalid control characters")
			return
		}
		if _, err := regexp.Compile(pattern); err != nil {
			o.mark(family, Manual, "invalid redirect regular expression")
			return
		}
		value := statute.RedirectRegex(pattern, replacement)
		code = fmt.Sprintf("statute.RedirectRegex(%q,%q)", pattern, replacement)
		if status == 301 && other == 308 {
			value.Permanent()
			code += ".Permanent()"
		} else if status == other {
			value.Status(status)
			code += fmt.Sprintf(".Status(%d)", status)
		}
		mw = value
	}
	chain.values = append(chain.values, mw)
	chain.code = append(chain.code, code)
	fields := []string{"regex", "replacement", fieldPermanent, "statusCode"}
	if family == redirectFamilyScheme {
		fields = []string{"scheme", "port", fieldPermanent, "forcePermanentRedirect"}
	}
	for _, field := range fields {
		o.mark(family+"."+field, Translated, "conditional redirect middleware with source captures, fallthrough and method statuses")
	}
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
