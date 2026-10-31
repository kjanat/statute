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
	chain.code = append(chain.code, textExpr(code))
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
		{"frameDeny", "X-Frame-Options", frameDenyValue},
		{"browserXssFilter", "X-Xss-Protection", xssBlockValue},
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
	burst, ok := sourceRateBurst(o)
	if !ok {
		return
	}
	delay, ttl := sourceRateTiming(rate)
	spec := rateLimitSpec{rate: strconv.FormatFloat(rate, 'g', -1, 64) + "/s", burst: burst, reserve: delay.String(), idle: ttl.String()}
	limiter := statute.RateLimit(spec.rate).Burst(burst).Reserve(spec.reserve).ExpireIdle(spec.idle).EvictOldest()
	if scalar(o.value("rateLimit.sourceCriterion.requestHost")) == trueValue {
		limiter.Per(statute.HostHeader)
		spec.key = hostRateKey
		o.mark("rateLimit.sourceCriterion.requestHost", Translated, "request Host bucket key")
	} else if header := o.text("rateLimit.sourceCriterion.requestHeaderName"); header != "" && literal(header) {
		limiter.FromHeader(header)
		spec.key, spec.header = headerRateKey, header
		o.mark("rateLimit.sourceCriterion.requestHeaderName", Translated, "request header bucket key")
	} else {
		selection, selectionCode, ok := sourceIPSelection(o, "rateLimit.sourceCriterion.ipStrategy")
		if !ok {
			return
		}
		limiter.SelectIP(selection)
		spec.selection = selectionCode
	}
	chain.values = append(chain.values, limiter)
	chain.code = append(chain.code, expr{rate: &spec})
	for _, key := range []string{"average", "period", "burst"} {
		o.mark("rateLimit."+key, Translated, "token refill, burst, delayed reservation and bounded idle eviction")
	}
	classifyRateCriterion(o)
}

type rateKey int

const (
	ipRateKey rateKey = iota
	hostRateKey
	headerRateKey
)

type rateLimitSpec struct {
	rate      string
	burst     int
	reserve   string
	idle      string
	key       rateKey
	selection string
	header    string
}

const rateLimitBase = "statute.RateLimit(rate).\nBurst(burst).\nReserve(reserve).\nExpireIdle(idle).\nEvictOldest()."

var rateLimitConstructors = map[string]struct{ signature, body string }{
	"makeRateLimit":       {"(\nrate string,\nburst int,\nreserve, idle string,\nselection statute.IPSelection,\n)", rateLimitBase + "\nSelectIP(selection)"},
	"makeHostRateLimit":   {"(rate string, burst int, reserve, idle string)", rateLimitBase + "\nPer(statute.HostHeader)"},
	"makeHeaderRateLimit": {"(\nrate string,\nburst int,\nreserve, idle, header string,\n)", rateLimitBase + "\nFromHeader(header)"},
}

func (s *rateLimitSpec) constructor() string {
	switch s.key {
	case hostRateKey:
		return "makeHostRateLimit"
	case headerRateKey:
		return "makeHeaderRateLimit"
	default:
		return "makeRateLimit"
	}
}

func (s *rateLimitSpec) selector() string {
	switch s.key {
	case hostRateKey:
		return "Per(statute.HostHeader)"
	case headerRateKey:
		return fmt.Sprintf("FromHeader(%q)", s.header)
	default:
		return "SelectIP(" + s.selection + ")"
	}
}

func (s *rateLimitSpec) inline() string {
	return fmt.Sprintf("statute.RateLimit(%q).Burst(%d).Reserve(%q).ExpireIdle(%q).EvictOldest().%s", s.rate, s.burst, s.reserve, s.idle, s.selector())
}

func (s *rateLimitSpec) call() string {
	args := fmt.Sprintf("%q,%d,%q,%q", s.rate, s.burst, s.reserve, s.idle)
	switch s.key {
	case hostRateKey:
	case headerRateKey:
		args += fmt.Sprintf(",%q", s.header)
	default:
		args += "," + s.selection
	}
	return s.constructor() + "(" + args + ")"
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

func headerFamily(path string) bool { return strings.HasPrefix(strings.ToLower(path), "headers.") }

func sourceRateTiming(rate float64) (time.Duration, time.Duration) {
	delay := 500 * time.Millisecond
	if rate >= 1 {
		delay = time.Second / (time.Duration(rate) * 2)
	}
	ttl := 2 * time.Second
	if rate < 1 {
		ttl = time.Duration(1+int(1/rate)) * time.Second
	}
	return delay, ttl
}

func sourceRateBurst(o *Object) (int, bool) {
	burst := 1
	if value := o.value("rateLimit.burst"); value != nil {
		var err error
		burst, err = integer(value)
		if err != nil {
			o.mark("rateLimit.burst", Manual, "burst must be an integer")
			return 0, false
		}
	}
	burst = max(1, burst)
	return burst, true
}
