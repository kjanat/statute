package migrate

import (
	"fmt"
	"strconv"
	"strings"

	statute "statute.kjanat.dev"
)

const (
	defaultTransportID = "default@internal"
	staticTransportID  = "setting:serversTransport@static"
)

// Traefik's transport and health defaults differ from Statute's. Emit them
// explicitly so an omitted source setting does not silently change behavior.
func sourceTransportDefaults() statute.Transport {
	return statute.Transport{MaxIdleConnsPerHost: 2, DialTimeout: "30s", IdleConnTimeout: "90s", TLSHandshakeTimeout: "10s", FlushInterval: "100ms"}
}

func transportLiteral(t statute.Transport) string {
	return fmt.Sprintf("statute.Transport{MaxIdleConnsPerHost:%d,DialTimeout:%q,IdleConnTimeout:%q,TLSHandshakeTimeout:%q,ResponseHeaderTimeout:%q,FlushInterval:%q,ServerName:%q,RootCAFiles:[]string{%s},ClientCertificate:statute.ClientCertificate{CertFile:%q,KeyFile:%q},InsecureSkipVerify:%t}", t.MaxIdleConnsPerHost, t.DialTimeout, t.IdleConnTimeout, t.TLSHandshakeTimeout, t.ResponseHeaderTimeout, t.FlushInterval, t.ServerName, quoted(t.RootCAFiles), t.ClientCertificate.CertFile, t.ClientCertificate.KeyFile, t.InsecureSkipVerify)
}

func (g *generator) transport(o *Object) {
	t := sourceTransportDefaults()
	if o.Provider == providerStatic {
		t.MaxIdleConnsPerHost = 200
	}
	for _, f := range []struct {
		key string
		dst *string
	}{{"serverName", &t.ServerName}, {"forwardingTimeouts.dialTimeout", &t.DialTimeout}, {"forwardingTimeouts.responseHeaderTimeout", &t.ResponseHeaderTimeout}, {"forwardingTimeouts.idleConnTimeout", &t.IdleConnTimeout}} {
		if v := o.text(f.key); v != "" && literal(v) {
			*f.dst = v
			o.mark(f.key, Translated, "explicit shared proxy and probe transport")
		}
	}
	transportOptions(o, &t)
	transportFiles(o, &t)
	if len(t.RootCAFiles) > 0 || t.ClientCertificate.CertFile != "" {
		g.r.Report.Checks = append(g.r.Report.Checks, Check{Tool: toolBindings, Code: "upstream-tls-material-required", Deferred: true})
	}
	if o.ready() {
		g.transports[o.Name+"@"+o.Provider] = t
		g.transportCode[o.Name+"@"+o.Provider] = transportLiteral(t)
	}
}

func transportOptions(o *Object, t *statute.Transport) {
	if n, err := integer(o.value("maxIdleConnsPerHost")); err == nil && n > 0 {
		t.MaxIdleConnsPerHost = n
		o.mark("maxIdleConnsPerHost", Exact, "per-backend idle connection limit")
	}
	if b, err := strconv.ParseBool(scalar(o.value("insecureSkipVerify"))); err == nil {
		t.InsecureSkipVerify = b
		o.mark("insecureSkipVerify", Translated, "explicit backend verification policy; canonical TLS lint still applies")
	}
	if scalar(o.value("disableHTTP2")) == falseValue {
		o.mark("disableHTTP2", Exact, "HTTP/2 transport negotiation remains enabled")
	}
}

func transportFiles(o *Object, t *statute.Transport) {
	if paths, ok := stringList(o.value("rootCAs"), o.Provider == providerDocker); ok {
		t.RootCAFiles = paths
		o.mark("rootCAs", Translated, "PEM file references replace system roots for both proxy and health traffic; material loaded only by generated program")
	}
	certs, ok := o.value("certificates").([]any)
	if !ok || len(certs) != 1 {
		return
	}
	c := asMap(certs[0])
	cert, key := stringValue(c["certFile"]), stringValue(c["keyFile"])
	if cert == "" || key == "" || !literal(cert) || !literal(key) {
		return
	}
	t.ClientCertificate = statute.ClientCertificate{CertFile: cert, KeyFile: key}
	o.mark("certificates[0].certFile", Translated, "client identity file reference; audit does not load material")
	o.mark("certificates[0].keyFile", Translated, "client key file reference; audit does not load material")
}

func serviceFlush(o *Object, t *statute.Transport) {
	const key = "loadBalancer.responseForwarding.flushInterval"
	if v := o.text(key); v != "" && literal(v) {
		t.FlushInterval = v
		o.mark(key, Translated, "periodic flush interval; streaming responses still flush immediately")
	}
}

func (g *generator) serviceHealth(o *Object, pool *statute.Pool) []string {
	const base = "loadBalancer.healthCheck"
	path := o.text(base + ".path")
	if path == "" || !literal(path) {
		return nil
	}
	h := statute.HealthCheck{Path: path, Interval: "30s", Timeout: "5s", Healthy: 1, Unhealthy: 1, ResolvePath: true, Redirects: "any_origin"}
	o.mark(base+".path", Translated, "pool probe path with source transition thresholds and timeout defaults")
	for _, f := range []struct {
		key string
		dst *string
	}{{"interval", &h.Interval}, {"timeout", &h.Timeout}, {"hostname", &h.Host}} {
		if v := o.text(base + "." + f.key); v != "" && literal(v) {
			*f.dst = v
			o.mark(base+"."+f.key, Translated, "pool-owned health policy")
		}
	}
	headers := g.healthBehavior(o, &h)
	pool.HealthCheck = h
	pool.RejectUnhealthy = true
	return []string{"HealthCheck:" + healthLiteral(h, headers)}
}

func (g *generator) healthBehavior(o *Object, h *statute.HealthCheck) map[string]string {
	headers := map[string]string{}
	const base = "loadBalancer.healthCheck"
	if n, err := integer(o.value(base + ".status")); err == nil && n >= 100 && n <= 599 {
		h.Statuses = []int{n}
		o.mark(base+".status", Translated, "explicit accepted probe status")
	}
	if method := o.text(base + ".method"); method != "" && literal(method) {
		h.Method = method
		o.mark(base+".method", Translated, "explicit probe method")
	}
	if mode := o.text(base + ".mode"); mode == protocolHTTP {
		o.mark(base+".mode", Exact, "HTTP probe")
	}
	follow, ok := sourceBoolValue(o.value(base+".followRedirects"), true)
	if !ok {
		o.mark(base+".followRedirects", Manual, "invalid redirect switch")
		return headers
	}
	if !follow {
		h.Redirects = "none"
	}
	o.mark(base+".followRedirects", Translated, "explicit probe redirect policy")
	return g.healthHeaders(o, h)
}

func healthLiteral(h statute.HealthCheck, bindings map[string]string) string {
	statuses := make([]string, len(h.Statuses))
	for i, n := range h.Statuses {
		statuses[i] = strconv.Itoa(n)
	}
	headers := make([]string, 0, len(h.Headers))
	for _, name := range keys(h.Headers) {
		headers = append(headers, fmt.Sprintf("%q:%s", name, bindings[name]))
	}
	return fmt.Sprintf("statute.HealthCheck{Path:%q,Interval:%q,Timeout:%q,Healthy:%d,Unhealthy:%d,Host:%q,Statuses:[]int{%s},ResolvePath:%t,Redirects:%q,Method:%q,Headers:map[string]string{%s}}", h.Path, h.Interval, h.Timeout, h.Healthy, h.Unhealthy, h.Host, strings.Join(statuses, ","), h.ResolvePath, h.Redirects, h.Method, strings.Join(headers, ","))
}

func (g *generator) dockerPoolPolicy(o *Object) {
	if g.r.Config.Docker == nil {
		o.note("$provider", Manual, "explicit Docker provider is required for exact-service policy registration")
		return
	}
	p := statute.Pool{Transport: g.transports[defaultTransportID]}
	source := defaultTransportID
	parts := serviceHost(o, &p)
	parts = append(parts, g.serviceHealth(o, &p)...)
	parts = append(parts, servicePassiveHealth(o, &p)...)
	parts = append(parts, fmt.Sprintf("RejectUnhealthy:%t", p.RejectUnhealthy))
	if name := o.text("loadBalancer.serversTransport"); name != "" {
		id := qualified(name, providerDocker)
		t, ok := g.transports[id]
		if !ok {
			o.note("$transport", Manual, "named transport must resolve to an audited file-provider transport")
			return
		}
		p.Transport = t
		source = id
		o.mark("loadBalancer.serversTransport", Translated, "exact-service compiled transport policy")
		if transport := g.objects["serversTransports:"+id]; transport != nil {
			transport.Generated = true
		}
	}
	serviceFlush(o, &p.Transport)
	parts = append(parts, "Transport:"+g.transportCall(o, source, p.Transport))
	if !o.readyWithBindings() {
		return
	}
	policy := statute.PoolPolicy{RejectUnhealthy: p.RejectUnhealthy, Transport: p.Transport, HealthCheck: p.HealthCheck, PassiveHealthCheck: p.PassiveHealthCheck, UpstreamHost: p.UpstreamHost}
	g.r.Config.Docker.PoolPolicy(o.Name+"@traefik", policy)
	g.docker += fmt.Sprintf(".PoolPolicy(%q,statute.PoolPolicy{%s})", o.Name+"@traefik", strings.Join(parts, ","))
}

func (g *generator) defaultTransport() {
	t := sourceTransportDefaults()
	t.MaxIdleConnsPerHost = 200
	g.transports[defaultTransportID] = t
	o := g.objects[staticTransportID]
	if o == nil {
		return
	}
	g.transport(o)
	if t, ok := g.transports[o.Name+"@"+o.Provider]; ok {
		g.transports[defaultTransportID] = t
		o.Generated = true
	}
}

func (g *generator) transportCall(service *Object, source string, t statute.Transport) string {
	name, origin := source, "serversTransports:"+source
	if source == defaultTransportID {
		name, origin = "default", "Traefik default serversTransport"
		if o := g.objects[staticTransportID]; o != nil && o.Generated {
			origin = o.identity()
		}
	}
	if transportLiteral(t) != transportLiteral(g.transports[source]) {
		name, origin = service.Name+"@"+service.Provider, service.identity()
	}
	return g.reuse(transportType, name, origin, transportLiteral(t))
}

func (g *generator) healthHeaders(o *Object, h *statute.HealthCheck) map[string]string {
	const base = "loadBalancer.healthCheck"
	headers := map[string]string{}
	if values := probeHeaderValues(o); values != nil {
		h.Headers = map[string]string{}
		for name, value := range values {
			text, ok := value.(string)
			if !ok || !literal(name) {
				continue
			}
			path := base + ".headers." + name
			o.mark(path, Translated, "probe request header")
			if text != "" {
				h.Headers[name], headers[name] = g.sensitiveValue(o, path, text)
			} else {
				h.Headers[name], headers[name] = "", strconv.Quote("")
			}
		}
	}
	return headers
}

func probeHeaderValues(o *Object) map[string]any {
	const path = "loadBalancer.healthCheck.headers"
	if values, ok := o.value(path).(map[string]any); ok {
		if len(values) == 0 {
			o.mark(path, Translated, "empty probe header map")
		}
		return values
	}
	values := map[string]any{}
	for _, field := range o.Fields {
		if strings.HasPrefix(strings.ToLower(field.Path), strings.ToLower(path)+".") {
			values[field.Path[len(path)+1:]] = o.value(field.Path)
		}
	}
	return values
}
