package migrate

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	statute "statute.kjanat.dev"
)

func (g *generator) policyHeader(o *Object, chain *middleware, path, name, value string, tlsOnly bool) {
	m := statute.SetResponseHeader(name, value)
	code := fmt.Sprintf("statute.SetResponseHeader(%q,%q)", name, value)
	if tlsOnly {
		m.OnHTTPS()
		code += ".OnHTTPS()"
	}
	chain.values = append(chain.values, m)
	chain.code = append(chain.code, code)
	o.mark(path, Translated, "native response-header policy")
}

func (g *generator) securityHeaders(o *Object, chain *middleware) {
	g.booleanSecurityHeaders(o, chain)
	g.stringSecurityHeaders(o, chain)
	g.hsts(o, chain)
	classifyConditionalHeaders(o)
}

func (g *generator) booleanSecurityHeaders(o *Object, chain *middleware) {
	for _, f := range []struct{ key, name, value string }{
		{"contentTypeNosniff", "X-Content-Type-Options", "nosniff"},
		{"frameDeny", "X-Frame-Options", "DENY"},
		{"browserXssFilter", "X-Xss-Protection", "1; mode=block"},
	} {
		path := "headers." + f.key
		if o.value(path) == nil {
			continue
		}
		enabled, err := strconv.ParseBool(scalar(o.value(path)))
		if err != nil {
			o.mark(path, Manual, "security-header switch requires a literal boolean")
			continue
		}
		if !enabled {
			o.mark(path, Exact, "header emission disabled")
			continue
		}
		g.policyHeader(o, chain, path, f.name, f.value, false)
	}
}

func (g *generator) stringSecurityHeaders(o *Object, chain *middleware) {
	for _, f := range []struct{ key, name string }{
		{fieldReferrerPolicy, "Referrer-Policy"},
		{"customFrameOptionsValue", "X-Frame-Options"},
		{"contentSecurityPolicy", "Content-Security-Policy"},
		{"permissionsPolicy", "Permissions-Policy"},
	} {
		path := "headers." + f.key
		value := o.text(path)
		if value == "" {
			if o.value(path) != nil {
				o.mark(path, Exact, "header emission disabled")
			}
			continue
		}
		if f.key == fieldReferrerPolicy && standardReferrerPolicy(value) {
			g.policyHeader(o, chain, path, f.name, value, false)
			continue
		}
		g.header(o, chain, "Response", f.name, value, path)
	}
}

func classifyConditionalHeaders(o *Object) {
	for _, key := range []string{"sslProxyHeaders", "hostsProxyHeaders", "allowedHosts", "sslRedirect", "sslTemporaryRedirect", "sslHost", "sslForceHost", "isDevelopment", "accessControlAllowCredentials", "accessControlAllowHeaders", "accessControlAllowMethods", "accessControlAllowOriginList", "accessControlAllowOriginListRegex", "accessControlExposeHeaders", "accessControlMaxAge", "addVaryHeader"} {
		path := "headers." + key
		if o.value(path) != nil {
			if emptyHeaderPolicy(o.value(path)) {
				o.mark(path, Exact, "conditional header feature disabled")
				continue
			}
			o.mark(path, Manual, "conditional host, TLS or CORS header behavior requires explicit native policy; fixed header operations are generated separately")
		}
	}
	for _, field := range o.Fields {
		if headerFamily(field.Path) && field.Reason == untranslatedReason {
			o.mark(field.Path, Manual, "header option needs conditional host, TLS or CORS policy beyond fixed response-header operations")
		}
	}
}

func emptyHeaderPolicy(value any) bool {
	switch v := value.(type) {
	case bool:
		return !v
	case string:
		return v == ""
	case []any:
		return len(v) == 0
	case map[string]any:
		return len(v) == 0
	case int:
		return v == 0
	default:
		return false
	}
}

func standardReferrerPolicy(value string) bool {
	switch value {
	case "no-referrer", "no-referrer-when-downgrade", "origin", "origin-when-cross-origin", "same-origin", "strict-origin", "strict-origin-when-cross-origin", "unsafe-url":
		return true
	default:
		return false
	}
}

func (g *generator) hsts(o *Object, chain *middleware) {
	const path = "headers.stsSeconds"
	if o.value(path) == nil {
		return
	}
	seconds, err := integer(o.value(path))
	if err != nil || seconds < 0 {
		o.mark(path, Manual, "HSTS max-age requires a nonnegative integer")
		return
	}
	var value strings.Builder
	fmt.Fprintf(&value, "max-age=%d", seconds)
	force := false
	for _, f := range []struct{ key, suffix string }{
		{"stsIncludeSubdomains", "; includeSubDomains"}, {"stsPreload", "; preload"}, {fieldForceSTSHeader, ""},
	} {
		key := "headers." + f.key
		if o.value(key) == nil {
			continue
		}
		b, err := strconv.ParseBool(scalar(o.value(key)))
		if err != nil {
			o.mark(key, Manual, "HSTS switch requires a literal boolean")
			continue
		}
		o.mark(key, Translated, "explicit HSTS directive")
		if f.key == fieldForceSTSHeader {
			force = b
		} else if b {
			value.WriteString(f.suffix)
		}
	}
	if seconds == 0 {
		o.mark(path, Exact, "HSTS emission disabled")
		return
	}
	g.policyHeader(o, chain, path, "Strict-Transport-Security", value.String(), !force)
}

func (g *generator) rateLimit(o *Object, chain *middleware) {
	if o.value("rateLimit") == nil && !hasFieldPrefix(o, "rateLimit.") {
		return
	}
	rate, ok := sourceRateLimit(o)
	if !ok {
		return
	}
	value := strconv.FormatFloat(rate, 'g', -1, 64) + "/s"
	limiter := statute.RateLimit(value)
	code := fmt.Sprintf("statute.RateLimit(%q)", value)
	if scalar(o.value("rateLimit.sourceCriterion.requestHost")) == trueValue {
		limiter.Per(statute.HostHeader)
		code += ".Per(statute.HostHeader)"
		o.mark("rateLimit.sourceCriterion.requestHost", Translated, "rate buckets use the request Host")
	} else {
		g.classifyClientIPPolicy(o)
	}
	chain.values = append(chain.values, limiter)
	chain.code = append(chain.code, code)
	o.mark("rateLimit.average", Translated, "native token refill rate")
	o.mark("rateLimit.period", Translated, "explicit refill period")
	classifyRateBurst(o, rate)
	o.note("$rate-delay", Manual, "native token exhaustion returns 429 immediately; source permits short delayed reservations before rejecting; choose request admission timing")
	o.note("$rate-capacity", Manual, "native limiter bounds client buckets and returns 503 for a new key at capacity; select client-resource admission policy explicitly")
	classifyRateCriterion(o)
}

func sourceRateLimit(o *Object) (float64, bool) {
	average, err := integer(o.value("rateLimit.average"))
	if err != nil || average <= 0 {
		o.mark("rateLimit", Manual, "rate limiting requires a positive literal average; disabled limits need no middleware")
		return 0, false
	}
	o.mark("rateLimit.average", Translated, "literal refill average; candidate also requires a valid period")
	period := time.Second
	if o.value("rateLimit.period") != nil {
		period, err = time.ParseDuration(o.text("rateLimit.period"))
		if err != nil || period < 0 {
			o.mark("rateLimit.period", Manual, "rate-limit period requires a positive literal duration")
			return 0, false
		}
		if period == 0 {
			period = time.Second
		}
	}
	rate := float64(average) / period.Seconds()
	if rate <= 0 || math.IsInf(rate, 0) {
		o.mark("rateLimit", Manual, "rate is outside the native finite rate range")
		return 0, false
	}
	return rate, true
}

func classifyRateBurst(o *Object, rate float64) {
	var err error
	burst := 1
	if o.value("rateLimit.burst") != nil {
		burst, err = integer(o.value("rateLimit.burst"))
	}
	burst = max(burst, 1)
	if err != nil || float64(burst) != math.Max(1, 2*rate) {
		o.note("$rate-burst", Manual, "native burst capacity is max(1, twice requests per second); source burst needs an explicit policy choice")
	}
	o.mark("rateLimit.burst", Translated, "source burst compared with native capacity")
}

func classifyRateCriterion(o *Object) {
	if value, ok := o.value("rateLimit.sourceCriterion").(map[string]any); ok && len(value) == 0 {
		o.mark("rateLimit.sourceCriterion", Exact, "default client-IP key")
	}
	if scalar(o.value("rateLimit.sourceCriterion.requestHost")) == falseValue {
		o.mark("rateLimit.sourceCriterion.requestHost", Exact, "Host key disabled")
	}
	for _, field := range o.Fields {
		if strings.HasPrefix(strings.ToLower(field.Path), "ratelimit.sourcecriterion") && field.Reason == untranslatedReason {
			o.mark(field.Path, Manual, "source header or IP-depth key selection differs from the native verified-client/Host keys")
		}
	}
	if o.value("rateLimit.distributed") != nil {
		if scalar(o.value("rateLimit.distributed")) == falseValue {
			o.mark("rateLimit.distributed", Exact, "local rate-limit state")
		} else {
			o.mark("rateLimit.distributed", Manual, "native limiter state is local to this route in one process")
		}
	}
}

func (g *generator) classifyClientIPPolicy(o *Object) {
	for _, ep := range g.entrypoints {
		ranges, _ := stringList(ep.value("forwardedHeaders.trustedIPs"), false)
		if scalar(ep.value("forwardedHeaders.insecure")) == trueValue || len(ranges) > 0 {
			o.note("$client-ip-policy", Manual, "source default client-IP policy uses the direct peer; native ACL/rate keys use verified forwarded identity for trusted proxies")
			return
		}
	}
}

func headerFamily(path string) bool { return strings.HasPrefix(strings.ToLower(path), "headers.") }
