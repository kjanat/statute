package migrate

import (
	"fmt"
	"math"
	"net"
	"slices"
	"strings"

	statute "statute.kjanat.dev"
)

func (g *generator) planListeners() {
	g.each(kindEntrypoints, func(o *Object) {
		g.entrypoints[o.Name] = o
		if scalar(o.value("asDefault")) == trueValue {
			g.defaultEntries = append(g.defaultEntries, o.Name)
		}
		g.contentEntries = append(g.contentEntries, o.Name)
	})
	slices.Sort(g.contentEntries)
	slices.Sort(g.defaultEntries)
	if len(g.defaultEntries) == 0 {
		g.defaultEntries = append([]string(nil), g.contentEntries...)
	}
	g.each("routers", g.planRouterListeners)
}

func (g *generator) planRouterListeners(o *Object) {
	entries, ok := stringList(o.value("entryPoints"), o.Provider == providerDocker)
	if !ok && o.value("entryPoints") != nil {
		return
	}
	if len(entries) == 0 {
		entries = append([]string(nil), g.defaultEntries...)
	}
	slices.Sort(entries)
	g.routerEntries[o] = slices.Compact(entries)
	for _, name := range entries {
		ep := g.entrypoints[name]
		if ep == nil {
			o.note("$entrypoint", Manual, "named entrypoint is absent from supplied static configuration")
			continue
		}
		if routerHasTLS(o) || ep.value("http.tls") != nil {
			g.entryTLS[name] = true
		}
	}
}

func (g *generator) classifyRouterPolicy(o *Object) {
	if _, err := integer(o.value("priority")); err == nil {
		o.mark("priority", Translated, "source priority orders file routes; Docker overlap is checked separately")
	}
	if syntax := o.text("ruleSyntax"); syntax == "v3" || syntax == defaultName {
		o.mark("ruleSyntax", Exact, "Traefik v3 literal matcher grammar")
	}
	entries := g.routerEntries[o]
	o.mark("entryPoints", Translated, "named listener route constraints")
	g.checkRouterProtocols(o, entries)
	// Specific TLS fields are consumed only when constructing selected listeners.
	if scalar(o.value("tls")) == trueValue {
		o.mark("tls", Translated, "TLS router attached only to generated TLS listeners")
	}
	if m, ok := o.value("tls").(map[string]any); ok && len(m) == 0 {
		o.mark("tls", Translated, "TLS router attached only to generated TLS listeners")
	}
}

func (g *generator) listener(o *Object) {
	addr := o.text("address")
	if !literal(addr) || addr == "" {
		return
	}
	addr, _ = strings.CutSuffix(addr, "/tcp")
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return
	}
	o.mark("address", Translated, "explicit listener address and protocol")
	if o.value("asDefault") != nil {
		o.mark("asDefault", Translated, "default entrypoint membership expanded into router inventory")
	}
	if g.listenerRedirect(o, addr) {
		return
	}
	var l *statute.Listener
	var code string
	if g.entryTLS[o.Name] || o.value("http.tls") != nil {
		opts, codes := g.listenerTLS(o)
		if len(opts) == 0 {
			o.note("$tls", Manual, "TLS listener has no usable supplied certificate or resolver")
			return
		}
		opts = append(opts, statute.HTTP2())
		codes = append(codes, "statute.HTTP2()")
		l = statute.HTTPS(addr, opts...)
		code = fmt.Sprintf("statute.HTTPS(%q,%s)", addr, strings.Join(codes, ","))
	} else {
		l = statute.HTTP(addr)
		code = fmt.Sprintf("statute.HTTP(%q)", addr)
	}
	l.Name(o.Name)
	code += fmt.Sprintf(".Name(%q)", o.Name)
	l, code = g.listenerIngress(o, l, code)
	g.r.Config.Listeners = append(g.r.Config.Listeners, l)
	g.listeners = append(g.listeners, code)
	o.Generated = true
}

func (g *generator) listenerRedirect(o *Object, addr string) bool {
	const base = "http.redirections.entryPoint"
	if o.value(base) == nil {
		return false
	}
	scheme := o.text(base + ".scheme")
	if scheme == "" {
		scheme = protocolHTTPS
	}
	if scheme != protocolHTTP && scheme != protocolHTTPS {
		o.mark(base, Manual, "invalid redirect scheme")
		return false
	}
	to := o.text(base + ".to")
	address := to
	if target := g.entrypoints[to]; target != nil {
		address = target.text("address")
	}
	_, port, err := net.SplitHostPort(address)
	if err != nil || !validPort(port) {
		o.mark(base+".to", Manual, "redirect target must name an entrypoint or valid port")
		return false
	}
	permanent, ok := sourceBoolValue(o.value(base+".permanent"), true)
	if !ok {
		o.mark(base+".permanent", Manual, "invalid redirect permanence")
		return false
	}
	order := math.MaxInt - 1
	if value := o.value(base + ".priority"); value != nil {
		var err error
		order, err = integer(value)
		if err != nil {
			o.mark(base+".priority", Manual, "invalid redirect priority")
			return false
		}
	}
	policy := statute.RedirectScheme(scheme).Port(port)
	code := fmt.Sprintf("statute.RedirectScheme(%q).Port(%q)", scheme, port)
	if permanent {
		policy.Permanent()
		code += ".Permanent()"
	}
	route := statute.Match("/*").OnListeners(o.Name).Priority(order).Handle(migrationNoop).With(policy)
	g.internalRoutes = append(g.internalRoutes, route)
	g.imports["net/http"] = true
	g.internalCode = append(g.internalCode, fmt.Sprintf("statute.Match(\"/*\").OnListeners(%q).Priority(%d).Handle(http.HandlerFunc(func(w http.ResponseWriter,_ *http.Request){w.WriteHeader(http.StatusTeapot)})).With(%s)", o.Name, order, code))
	for _, key := range []string{"to", "scheme", fieldPermanent, "priority"} {
		o.mark(base+"."+key, Translated, "listener-scoped conditional redirect at source priority")
	}
	return false
}

func (g *generator) listenerTLS(ep *Object) ([]statute.ListenerOption, []string) {
	var opts []statute.ListenerOption
	var codes []string
	if g.certFile != "" {
		opts = append(opts, statute.StaticTLS(g.certFile, g.keyFile))
		codes = append(codes, fmt.Sprintf("statute.StaticTLS(%q,%q)", g.certFile, g.keyFile))
	}
	domains := map[string][]string{}
	g.each("routers", func(o *Object) {
		if !slices.Contains(g.routerEntries[o], ep.Name) {
			return
		}
		owner, base := routerTLSSource(o, ep)
		resolver := owner.text(base + ".certResolver")
		if resolver != "" {
			domains[resolver] = append(domains[resolver], g.tlsDomains(o, ep)...)
			owner.mark(base+".certResolver", Translated, "named ACME resolver selected by this router")
			owner.reference(base+".certResolver", "certificatesresolvers", qualified(resolver, providerStatic))
		}
	})
	for _, name := range keys(domains) {
		ds := domains[name]
		slices.Sort(ds)
		ds = slices.Compact(ds)
		if option, code := g.acme(name, ds); option != nil {
			opts = append(opts, option)
			codes = append(codes, code)
		}
	}
	if len(opts) > 0 {
		if m, ok := ep.value("http.tls").(map[string]any); ok && len(m) == 0 {
			ep.mark("http.tls", Translated, "default TLS enabled for attached routers")
		}
	}
	opts, codes = g.tlsPolicy(ep, opts, codes)
	return opts, codes
}

func (g *generator) tlsDomains(o, ep *Object) []string {
	owner, base := routerTLSSource(o, ep)
	path := base + ".domains"
	if values, ok := owner.value(path).([]any); ok {
		out, ok := certificateDomains(values)
		if !ok {
			return nil
		}
		owner.mark(path, Translated, "explicit certificate domain coverage; SAN groups become native managed domain sources")
		return out
	}
	ms, err := parseRuleHosts(o)
	if err != nil {
		return nil
	}
	return ms
}

func routerHasTLS(o *Object) bool {
	return o.value("tls") != nil || (o.Provider == providerDocker && hasFieldPrefix(o, "tls."))
}

func (g *generator) checkRouterProtocols(o *Object, entries []string) {
	for _, name := range entries {
		ep := g.entrypoints[name]
		if g.entryTLS[name] && !routerHasTLS(o) && ep.value("http.tls") == nil {
			o.note("$protocol", Manual, "plain HTTP and TLS routers share one source entrypoint; native listeners select a single protocol")
		}
	}
}

func certificateDomains(values []any) ([]string, bool) {
	var out []string
	for _, value := range values {
		m := asMap(value)
		main := stringValue(m["main"])
		if main == "" || !literal(main) {
			return nil, false
		}
		for key := range m {
			if key != "main" && key != "sans" {
				return nil, false
			}
		}
		out = append(out, main)
		if m["sans"] == nil {
			continue
		}
		sans, ok := stringList(m["sans"], false)
		if !ok {
			return nil, false
		}
		out = append(out, sans...)
	}
	return out, true
}
