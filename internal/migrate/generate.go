package migrate

import (
	"errors"
	"fmt"
	"go/format"
	"io"
	"math"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	statute "statute.kjanat.dev"
	"statute.kjanat.dev/internal/docker"
)

type middleware struct {
	values []statute.Middleware
	code   []expr
	parts  []middlewarePart
}

type generator struct {
	r               *Result
	listeners       []fragment
	pools           map[string]string
	routes          []fragment
	internalRoutes  []*statute.Route
	internalCode    []fragment
	disabledRouters map[*Object]bool
	docker          string
	dockerOrigin    string
	objects         map[string]*Object
	chains          map[string]middleware
	visiting        map[string]bool
	attempted       map[string]bool
	transports      map[string]statute.Transport
	transportCode   map[string]string
	hosts           map[string]bool
	certFile        string
	keyFile         string
	imports         map[string]bool
	helpers         map[string]string
	candidates      map[string]fragment
	entrypoints     map[string]*Object
	entryTLS        map[string]bool
	routerEntries   map[*Object][]string
	contentEntries  []string
	observability   string
	defaults        string
	defaultEntries  []string
	entryDefaults   middleware
	reusables       map[reusableKey]*reusable
	simplePools     map[string]string
	reusableNames   map[string]bool
}

func generate(r *Result) error {
	g := &generator{r: r, pools: map[string]string{}, objects: map[string]*Object{}, chains: map[string]middleware{}, visiting: map[string]bool{}, attempted: map[string]bool{}, transports: map[string]statute.Transport{}, transportCode: map[string]string{}, hosts: map[string]bool{}}
	g.imports = map[string]bool{}
	g.helpers = map[string]string{}
	g.candidates = map[string]fragment{}
	g.entrypoints = map[string]*Object{}
	g.entryTLS = map[string]bool{}
	g.routerEntries = map[*Object][]string{}
	g.disabledRouters = map[*Object]bool{}
	g.reusables = map[reusableKey]*reusable{}
	g.simplePools = map[string]string{}
	g.reusableNames = map[string]bool{}
	r.Config.Upstreams = statute.Upstreams{}
	r.Config.PrioritizeRoutes = true
	g.indexObjects()
	g.each(kindTLS, g.certificates)
	g.planListeners()
	g.defaultTransport()
	g.defaults = g.listenerDefaults()
	for _, o := range r.Report.Objects {
		g.infrastructure(o)
	}
	g.each("middlewares", func(o *Object) { g.chain(o) })
	g.entrypointMiddleware()
	g.each("services", g.service)
	g.each(kindContainer, g.container)
	routers := g.orderedRouters()
	for _, o := range routers {
		g.router(o)
	}
	if len(g.entryDefaults.values) > 0 {
		for i, route := range g.internalRoutes {
			route.WithOuter(g.entryDefaults.values...)
			g.internalCode[i].code += withCalls("WithOuter", g.entryDefaults.parts)
		}
	}
	r.Config.Routes = append(r.Config.Routes, g.internalRoutes...)
	g.routes = append(g.routes, g.internalCode...)
	r.Report.GeneratedRoutes += len(g.internalRoutes)
	g.unusedTLSPolicies()
	g.checkPrecedence()
	if r.Config.Docker != nil {
		g.registerMiddleware(routers)
	}
	r.Report.Hosts = len(g.hosts)
	g.validate()
	g.bindingChecks()
	finish(&r.Report)
	source, err := g.source()
	if err != nil {
		return errors.New("cannot format generated Go")
	}
	r.Go = source
	return nil
}

func (g *generator) indexObjects() {
	for _, o := range g.r.Report.Objects {
		id := o.identity()
		if old := g.objects[id]; old != nil {
			old.note("$identity", Manual, "duplicate provider-qualified identity")
			o.note("$identity", Manual, "duplicate provider-qualified identity")
		} else {
			g.objects[id] = o
		}
	}
}

func (g *generator) each(kind string, fn func(*Object)) {
	for _, o := range g.r.Report.Objects {
		if o.Kind == kind {
			fn(o)
		}
	}
}

func (g *generator) infrastructure(o *Object) {
	switch o.Kind {
	case kindEntrypoints:
		g.listener(o)
	case kindProviders:
		g.provider(o)
	case "serversTransports":
		g.transport(o)
	case protocolTCP, "udp":
		o.mark("", OutOfScope, "TCP/UDP migration is outside the HTTP MVP")
	case "setting":
		g.setting(o)
	case kindTLS, "certificatesresolvers":
		// These objects are consumed by the listeners that select them.
	}
}

func (g *generator) orderedRouters() []*Object {
	var routers []*Object
	g.each("routers", func(o *Object) { routers = append(routers, o) })
	slices.SortFunc(routers, func(a, b *Object) int {
		pa, pb := priority(a), priority(b)
		if pa > pb {
			return -1
		}
		if pa < pb {
			return 1
		}
		return strings.Compare(a.identity(), b.identity())
	})
	return routers
}

func (g *generator) provider(o *Object) {
	switch o.Name {
	case providerFile:
		g.fileProvider(o)
	case providerDocker:
		g.dockerProvider(o)
	default:
		o.mark("", OutOfScope, "provider outside the file/Docker MVP")
		o.note("$parity", Manual, "external provider traffic is not generated")
	}
}

func (g *generator) dockerProvider(o *Object) {
	d := statute.Docker().TraefikLabels().RouterConstraints()
	d.DefaultListeners(g.defaultEntries...)
	code := "statute.Docker().TraefikLabels().RouterConstraints().DefaultListeners(" + quoted(g.defaultEntries) + ")" + dockerEndpoint(o, d) + dockerNetwork(o, d)
	exposed := true
	if v := o.value("exposedByDefault"); v != nil {
		var err error
		exposed, err = strconv.ParseBool(scalar(v))
		if err != nil {
			return
		}
		o.mark("exposedByDefault", Translated, "explicit exposure policy; implicit defaultRule discovery requires review")
	}
	if exposed {
		d.ExposedByDefault()
		code += ".ExposedByDefault()"
		o.note("$defaultRule", Manual, "Traefik implicit Host rules differ from Statute discovery; inventory every exposed container")
	}
	if o.value("watch") != nil {
		if scalar(o.value("watch")) == trueValue {
			o.mark("watch", Translated, "event-driven Docker discovery")
		} else {
			o.mark("watch", Manual, "Statute Docker discovery cannot freeze an initial snapshot")
		}
	}
	g.r.Config.Docker = d
	g.docker = code
	g.dockerOrigin = o.identity()
	o.Generated = true
}

func dockerEndpoint(o *Object, d *statute.DockerConfig) string {
	v := o.text("endpoint")
	if v == "" || !literal(v) {
		return ""
	}
	u, err := url.Parse(v)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	d.Endpoint(v)
	o.mark("endpoint", Exact, "explicit Docker provider endpoint")
	return fmt.Sprintf(".Endpoint(%q)", v)
}

func dockerNetwork(o *Object, d *statute.DockerConfig) string {
	v := o.text("network")
	if v == "" || !literal(v) {
		return ""
	}
	d.Network(v)
	o.mark("network", Exact, "explicit Docker network")
	return fmt.Sprintf(".Network(%q)", v)
}

func (g *generator) service(o *Object) {
	if o.Provider == providerDocker {
		g.dockerService(o)
		return
	}
	if o.Provider != providerFile {
		return
	}
	pool := statute.Pool{Transport: g.transports[defaultTransportID]}
	source := defaultTransportID
	backends, ok := serviceBackends(o, &pool)
	if !ok {
		return
	}
	parts := []string{"Backends:[]statute.Backend{" + strings.Join(backends, ",") + "}", fmt.Sprintf("AllowEmpty:%t", pool.AllowEmpty)}
	parts = append(parts, serviceHost(o, &pool)...)
	parts = append(parts, g.serviceHealth(o, &pool)...)
	parts = append(parts, servicePassiveHealth(o, &pool)...)
	parts = append(parts, fmt.Sprintf("RejectUnhealthy:%t", pool.RejectUnhealthy))
	if name := o.text("loadBalancer.serversTransport"); name != "" {
		o.reference("loadBalancer.serversTransport", "transport", name)
		id := qualified(name, o.Provider)
		t, found := g.transports[id]
		if !found {
			o.note("$transport", Manual, "transport reference is missing or has blocking fields")
			return
		}
		pool.Transport = t
		source = id
		o.mark("loadBalancer.serversTransport", Translated, "provider-qualified code-owned pool transport")
	}
	serviceFlush(o, &pool.Transport)
	transport := g.transportCall(o, source, pool.Transport)
	parts = append(parts, "Transport:"+transport)
	if !o.readyWithBindings() {
		return
	}
	g.registerPool(o.Name+"@file", pool, "statute.Pool{"+strings.Join(parts, ",")+"}", transport)
	o.Generated = true
	if name := o.text("loadBalancer.serversTransport"); name != "" {
		if transport := g.objects["serversTransports:"+qualified(name, o.Provider)]; transport != nil {
			transport.Generated = true
		}
	}
}

func (g *generator) registerPool(id string, pool statute.Pool, code, transport string) {
	g.r.Config.Upstreams[id] = pool
	g.pools[id] = code
	if len(pool.Backends) == 1 && reflect.DeepEqual(pool, statute.Pool{Backends: []statute.Backend{{Address: pool.Backends[0].Address}}, Transport: pool.Transport}) {
		g.simplePools[id] = fmt.Sprintf("makePool(%q,%s)", pool.Backends[0].Address, transport)
	}
}

func serviceBackends(o *Object, pool *statute.Pool) ([]string, bool) {
	servers, ok := o.value("loadBalancer.servers").([]any)
	if !ok {
		return nil, false
	}
	if len(servers) == 0 {
		pool.AllowEmpty = true
		o.mark("loadBalancer.servers", Translated, "empty upstream returns 503")
		return nil, true
	}
	var backends []string
	for i, item := range servers {
		address, ok := backendAddress(stringValue(asMap(item)["url"]))
		if !ok {
			return nil, false
		}
		pool.Backends = append(pool.Backends, statute.Backend{Address: address})
		backends = append(backends, fmt.Sprintf("{Address:%q}", address))
		o.mark(fmt.Sprintf("loadBalancer.servers[%d].url", i), Translated, "native backend URL; no credentials, query or base-path rewriting")
	}
	return backends, true
}

func backendAddress(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || !literal(raw) {
		return "", false
	}
	if u.Scheme != protocolHTTP && u.Scheme != protocolHTTPS {
		return "", false
	}
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	if strings.TrimPrefix(u.Path, "/") != "" {
		return "", false
	}
	return u.Scheme + "://" + u.Host, true
}

func serviceHost(o *Object, pool *statute.Pool) []string {
	v := o.value("loadBalancer.passHostHeader")
	if v == nil {
		return nil
	}
	b, err := strconv.ParseBool(scalar(v))
	if err != nil {
		return nil
	}
	o.mark("loadBalancer.passHostHeader", Translated, "pool-owned upstream Host policy")
	if b {
		return nil
	}
	pool.UpstreamHost = statute.TargetHost
	return []string{"UpstreamHost:statute.TargetHost"}
}

func (g *generator) dockerService(o *Object) {
	for _, key := range []string{dockerServerPort, "loadbalancer.server.scheme"} {
		classifyDockerServiceField(o, key)
	}
	g.dockerPoolPolicy(o)
	if o.ready() {
		o.Generated = true
	}
}

func classifyDockerServiceField(o *Object, key string) {
	v := scalar(o.value(key))
	if v == "" || !literal(v) {
		return
	}
	switch key {
	case "loadbalancer.server.scheme":
		if v != protocolHTTP && v != protocolHTTPS {
			o.mark(key, Unsupported, "backend protocol is unsupported")
			return
		}
	case dockerServerPort:
		if !validPort(v) {
			return
		}
	case "loadbalancer.healthcheck.interval", "loadbalancer.healthcheck.timeout":
		if !positiveDuration(v) {
			return
		}
	}
	o.mark(key, Exact, "retained Compose label consumed by TraefikLabels at runtime")
}

func validPort(value string) bool {
	n, err := strconv.Atoi(value)
	return err == nil && n > 0 && n <= 65535
}

func positiveDuration(value string) bool {
	d, err := time.ParseDuration(value)
	return err == nil && d > 0
}

func (g *generator) container(o *Object) {
	for _, f := range []string{"enable", "docker.network"} {
		v := scalar(o.value(f))
		if v == "" || !literal(v) {
			continue
		}
		if f == "enable" {
			if _, err := strconv.ParseBool(v); err != nil {
				continue
			}
		}
		o.mark(f, Exact, "retained Docker label; no daemon inspection performed")
	}
	if o.ready() {
		o.Generated = true
	}
}

func qualified(name, provider string) string {
	if strings.Contains(name, "@") {
		return name
	}
	return name + "@" + provider
}

func priority(o *Object) int {
	n, err := integer(o.value("priority"))
	if err == nil && n != 0 {
		return n
	}
	return len(o.text("rule"))
}

func (g *generator) router(o *Object) {
	g.r.Report.Routes++
	if g.disabledRouters[o] {
		o.mark("", OutOfScope, "router disabled by source strict TLS conflict policy")
		o.Generated = true
		return
	}
	ms, ok := g.parseRouter(o)
	if !ok {
		return
	}
	if qualified(o.text("service"), o.Provider) == "api@internal" {
		o.mark("", OutOfScope, "Traefik API and dashboard routes are outside HTTP proxy migration")
		return
	}
	g.classifyRouterPolicy(o)
	refs, valid := g.middlewareRefs(o)
	if !valid {
		o.note("$middlewares", Manual, "malformed middleware references")
		return
	}
	chain := g.routerMiddleware(o, refs)
	if o.Provider == providerDocker {
		g.dockerRouter(o, len(ms))
		return
	}
	if o.Provider != providerFile {
		return
	}
	name := qualified(o.text("service"), o.Provider)
	o.reference("service", "service", o.text("service"))
	if !g.serviceAvailable(name) {
		o.mark("service", Exact, "provider-qualified service reference inventoried; unresolved service is reported separately")
		o.note("$service", Manual, "service reference is missing, unsupported or blocked")
		return
	}
	o.mark("service", Translated, "provider-qualified native route action")

	if !o.ready() {
		return
	}
	for _, m := range ms {
		m.Priority = priority(o)
		m.ListenerNames = append([]string(nil), g.routerEntries[o]...)
		g.proxyRoute(m, name, chain, o.identity())
	}
	g.markMiddlewaresUsed(refs, o.Provider)
	o.Generated = true
}

func (g *generator) parseRouter(o *Object) ([]docker.Matcher, bool) {
	rule := o.text("rule")
	ms, err := docker.ParseRule(rule)
	if err != nil || !literal(rule) {
		g.r.Report.UnknownRouteCounts++
		o.mark("rule", Unsupported, unsupportedRuleReason(rule))
		g.accountBlockedRouter(o)
		return nil, false
	}
	for _, m := range ms {
		if m.Host != "" {
			g.hosts[m.Host] = true
		}
	}
	o.mark("rule", Translated, "shared Traefik parser; byte-prefix and host semantics checked before generation")
	return ms, true
}

func (g *generator) routerMiddleware(o *Object, refs []string) middleware {
	var chain middleware
	if o.Provider != providerDocker {
		chain.values = append(chain.values, g.entryDefaults.values...)
		chain.parts = append(chain.parts, g.entryDefaults.parts...)
	}
	g.checkDefaultMiddleware(o)
	for _, ref := range refs {
		if !literal(ref) {
			o.note("$middleware", Manual, "unresolved middleware reference; interpolation is not evaluated")
			continue
		}
		o.reference("middlewares", "middleware", ref)
		id := qualified(ref, o.Provider)
		mw, found := g.chains[id]
		if !found {
			o.mark("middlewares", Exact, "provider-qualified dependencies are reported in route impact findings")
			o.note("$middleware:"+id, Manual, "route blocked by middleware "+id+"; see its field findings")
			continue
		}
		if len(chain.values)+len(mw.values) > 256 {
			o.note("$size", Unsupported, "assembled middleware chain exceeds 256 operations")
			return chain
		}
		chain.values = append(chain.values, mw.values...)
		chain.parts = append(chain.parts, mw.parts...)
	}
	if len(refs) > 0 {
		o.mark("middlewares", Translated, "ordered code-owned middleware chain, scoped to this router")
	}
	return chain
}

func (g *generator) dockerRouter(o *Object, count int) {
	if !g.inferDockerService(o) {
		return
	}
	o.reference("service", "service", o.text("service"))
	if name := o.text("service"); name != "" {
		if !strings.Contains(name, "@") {
			o.mark("service", Exact, "retained Docker service reference")
		} else if g.bindService(o, name) {
			o.mark("service", Translated, "code-owned handler or static upstream binding")
			if o.ready() {
				o.Generated = true
				g.r.Report.GeneratedRoutes += count
			}
			return
		} else {
			o.note("$service", Manual, "referenced service is absent or blocked")
			return
		}
	}
	if g.r.Config.Docker == nil {
		o.note("$provider", Manual, "supply static Docker provider settings before enabling discovery")
	}
	if o.value("priority") != nil {
		o.mark("priority", Exact, "source priority is checked against overlapping routes")
	}
	g.dockerDependencies(o)
	if o.ready() {
		o.Generated = true
		g.r.Report.GeneratedRoutes += count
	}
}

func (g *generator) dockerDependencies(o *Object) {
	name := o.text("service")
	service := g.objects["services:"+qualified(name, providerDocker)]
	if name == "" || service == nil {
		o.note("$service", Manual, "implicit or missing Docker service requires port and service-name review")
		return
	}
	if !service.ready() || service.value(dockerServerPort) == nil {
		o.note("$service", Manual, "Docker service has unresolved policy or port")
	}
	enabled := false
	for _, candidate := range g.r.Report.Objects {
		if candidate.Kind == kindContainer && candidate.Source == o.Source {
			b, err := strconv.ParseBool(scalar(candidate.value("enable")))
			enabled = err == nil && b
		}
	}
	if !enabled {
		o.note("$exposure", Manual, "router lacks explicit container opt-in; review effective exposure and default rules")
	}
}

func (g *generator) proxyRoute(m docker.Matcher, name string, chain middleware, origin string) {
	hosts := hostSpellings(m.Host)
	constructor := "Match"
	newRoute := statute.Match
	switch m.PathKind {
	case docker.PathExact:
		constructor = "MatchExact"
		newRoute = statute.MatchExact
	case docker.PathByte:
		constructor = "MatchPrefix"
		newRoute = statute.MatchPrefix
	default:
	}
	route := newRoute(m.Path).ProxyTo(name).With(chain.values...).Priority(m.Priority).OnListeners(m.ListenerNames...)
	code := fmt.Sprintf("statute.%s(%q).ProxyTo(%q).Priority(%d).OnListeners(%s)", constructor, m.Path, name, m.Priority, quoted(m.ListenerNames))
	if g.discoveredService(name) {
		target := strings.TrimSuffix(name, "@docker") + "@traefik"
		route = newRoute(m.Path).ProxyToDiscovered(target).With(chain.values...).Priority(m.Priority).OnListeners(m.ListenerNames...)
		code = fmt.Sprintf("statute.%s(%q).ProxyToDiscovered(%q).Priority(%d).OnListeners(%s)", constructor, m.Path, target, m.Priority, quoted(m.ListenerNames))
	}
	if name == serviceNoop {
		route = newRoute(m.Path).Handle(migrationNoop).With(chain.values...).Priority(m.Priority).OnListeners(m.ListenerNames...)
		code = fmt.Sprintf("statute.%s(%q).Handle(%s).Priority(%d).OnListeners(%s)", constructor, m.Path, g.noopHandler(), m.Priority, quoted(m.ListenerNames))
	}
	if len(m.PeerIPCIDRs) > 0 {
		route.PeerIPs(m.PeerIPCIDRs...)
		code += ".PeerIPs(" + quoted(m.PeerIPCIDRs) + ")"
	}
	item := fragment{origin: origin, code: code + withCalls("With", chain.parts)}
	if len(hosts) > 0 {
		route.Hosts(hosts...)
		item = fragment{origin: origin, code: code, hosts: hosts, tail: withCalls("With", chain.parts)}
	}
	g.r.Config.Routes = append(g.r.Config.Routes, route)
	g.routes = append(g.routes, item)
	g.r.Report.GeneratedRoutes += max(1, len(hosts))
}

func hostSpellings(host string) []string {
	if host == "" {
		return nil
	}
	out := []string{host, host + "."}
	if before, ok := strings.CutSuffix(host, "."); ok {
		out = append(out, before)
	}
	return out
}

func quoted(values []string) string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = strconv.Quote(v)
	}
	return strings.Join(out, ",")
}

func middlewareRefs(o *Object) ([]string, bool) {
	v := o.value("middlewares")
	if v == nil {
		return nil, true
	}
	return stringList(v, o.Provider == providerDocker)
}

func (g *generator) checkPrecedence() {
	g.r.Report.Checks = append(g.r.Report.Checks, Check{Tool: "routing", Code: "source-route-priority-enabled"})
}

func (g *generator) validate() {
	resolved, err := statute.Resolve(g.r.Config)
	if err != nil {
		g.r.Report.Checks = append(g.r.Report.Checks, Check{Tool: toolResolve, Code: "invalid-generated-config", Blocking: true})
		for _, tool := range []string{toolLint, toolExport, toolGraph, toolTopology} {
			g.r.Report.Checks = append(g.r.Report.Checks, Check{Tool: tool, Code: "not-run-invalid-generated-config", Blocking: true})
		}
		return
	}
	g.r.Report.Checks = append(g.r.Report.Checks, Check{Tool: toolResolve, Code: "ok"})
	findings, err := statute.Lint(g.r.Config)
	if err != nil {
		g.r.Report.Checks = append(g.r.Report.Checks, Check{Tool: toolLint, Code: "failed", Blocking: true})
	}
	for _, f := range findings {
		g.r.Report.Checks = append(g.r.Report.Checks, Check{Tool: toolLint, Code: f.Code, Blocking: f.Severity == statute.SeverityError})
	}
	for _, check := range []struct {
		name string
		fn   func(statute.Config, io.Writer) error
	}{{toolExport, statute.Export}, {toolGraph, statute.GraphDOT}} {
		code := "ok"
		failed := check.fn(g.r.Config, io.Discard) != nil
		if failed {
			code = "failed"
		}
		g.r.Report.Checks = append(g.r.Report.Checks, Check{Tool: check.name, Code: code, Blocking: failed})
	}
	g.checkTopology(len(resolved.Routes)+len(resolved.FallbackRoutes), len(resolved.Upstreams))
	g.checkSourceTopology(resolved)
	g.checkDockerTopology(resolved)
	g.checkDockerExtraction(resolved)
}

func (g *generator) checkTopology(routes, pools int) {
	want := 0
	for _, o := range g.r.Report.Objects {
		if o.Kind != "routers" || o.Provider != providerFile || !o.Generated || g.disabledRouters[o] {
			continue
		}
		ms, err := docker.ParseRule(o.text("rule"))
		if err != nil {
			continue
		}
		for _, m := range ms {
			want += max(1, len(hostSpellings(m.Host)))
		}
	}
	failed := routes != want+len(g.internalRoutes) || pools != len(g.pools)
	code := "generated-route-and-pool-counts-match"
	if failed {
		code = "generated-topology-mismatch"
	}
	g.r.Report.Checks = append(g.r.Report.Checks, Check{Tool: toolTopology, Code: code, Blocking: failed})
}

func (g *generator) source() ([]byte, error) {
	var b strings.Builder
	if err := g.helperSource(&b); err != nil {
		return nil, err
	}
	for _, o := range g.r.Report.Objects {
		for _, f := range o.Fields {
			if blocked(f.Classification) {
				fmt.Fprintf(&b, "// TODO %q %q: %s\n", o.identity(), f.Path, f.Reason)
			}
		}
	}
	if err := g.configurationSource(&b); err != nil {
		return nil, err
	}
	helpers, err := g.reusableSource(b.String())
	if err != nil {
		return nil, err
	}
	b.WriteString(helpers + g.poolConstructor())
	b.WriteString("\nfunc main(){\n")
	if g.hasImplementationGaps() {
		b.WriteString("// TODO: resolve every blocking audit finding and validate the completed configuration.\npanic(\"migration incomplete: resolve blocking findings before serving\")\n")
	} else {
		b.WriteString("statute.Main(configuration())\n")
	}
	b.WriteString("}\n")
	header, err := g.importHeader(b.String())
	if err != nil {
		return nil, err
	}
	return format.Source([]byte(header + b.String()))
}

func (g *generator) helperSource(b *strings.Builder) error {
	for _, name := range keys(g.helpers) {
		b.WriteString(g.helpers[name])
		b.WriteString("\n")
	}
	for _, name := range keys(g.candidates) {
		candidate := g.candidates[name]
		code, err := layoutExpr(candidate.code)
		if err != nil {
			return err
		}
		fmt.Fprintf(b, "// %s is incomplete; see the audit for %s.\nfunc %s() []statute.Middleware {\nreturn %s\n}\n", name, strconv.Quote(candidate.origin), name, code)
	}
	return nil
}

func (g *generator) importHeader(body string) (string, error) {
	used, err := usedImports("package main\n" + body)
	if err != nil {
		return "", err
	}
	for name := range g.imports {
		used[name] = true
	}
	var header strings.Builder
	header.WriteString("// Generated migration configuration. Review the accompanying audit before deployment.\npackage main\n\nimport (\n")
	for _, name := range keys(used) {
		fmt.Fprintf(&header, "%q\n", name)
	}
	header.WriteString("\nstatute \"statute.kjanat.dev\"\n)\n")
	return header.String(), nil
}

func (g *generator) configurationSource(b *strings.Builder) error {
	defaults, err := layoutExpr(g.defaults)
	if err != nil {
		return err
	}
	b.WriteString("\nfunc configuration() statute.Config {\nreturn statute.Config{\nPrioritizeRoutes: true,\nDefaults: " + defaults + ",\n")
	if g.observability != "" {
		observability, err := layoutExpr(g.observability)
		if err != nil {
			return err
		}
		b.WriteString("Observability: " + observability + ",\n")
	}
	var sections strings.Builder
	for _, section := range []struct {
		field, name, typ string
		items            []fragment
	}{
		{"Listeners", "generatedListeners", "statute.Listeners", g.listeners},
		{"Upstreams", "generatedUpstreams", "statute.Upstreams", g.poolFragments()},
		{"Routes", "generatedRoutes", "statute.Routes", g.routes},
	} {
		if len(section.items) == 0 {
			continue
		}
		b.WriteString(section.field + ": " + section.name + "(),\n")
		if err := writeSection(&sections, section.name, section.typ, section.items); err != nil {
			return err
		}
	}
	if g.docker != "" {
		docker, err := layoutExpr(g.docker)
		if err != nil {
			return err
		}
		b.WriteString("Docker: generatedDocker(),\n")
		sections.WriteString("\nfunc generatedDocker() *statute.DockerConfig {\n" + originComment(g.dockerOrigin) + "\nreturn " + docker + "\n}\n")
	}
	b.WriteString("}\n}\n")
	b.WriteString(sections.String())
	return nil
}

func (g *generator) poolFragments() []fragment {
	out := make([]fragment, 0, len(g.pools))
	for _, name := range keys(g.pools) {
		code := g.pools[name]
		if call, ok := g.simplePools[name]; ok && len(g.simplePools) > 1 {
			code = call
		}
		out = append(out, fragment{origin: "services:" + name, code: code, key: strconv.Quote(name) + ": "})
	}
	return out
}

func (g *generator) poolConstructor() string {
	if len(g.simplePools) < 2 {
		return ""
	}
	return "\nfunc makePool(address string, transport statute.Transport) statute.Pool {\nreturn statute.Pool{\nBackends: []statute.Backend{{Address: address}},\nTransport: transport,\n}\n}\n"
}

func writeSection(b *strings.Builder, name, typ string, items []fragment) error {
	b.WriteString("\nfunc " + name + "() " + typ + " {\nreturn " + typ + "{\n")
	for i, item := range mergeHosts(items) {
		code, err := layoutExpr(item.render())
		if err != nil {
			return err
		}
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(originComment(item.origin) + "\n" + item.key + code + ",\n")
	}
	b.WriteString("}\n}\n")
	return nil
}

func (g *generator) priorityCode(n int) string {
	switch n {
	case math.MaxInt:
		g.imports["math"] = true
		return "math.MaxInt"
	case math.MaxInt - 1:
		g.imports["math"] = true
		return "math.MaxInt - 1"
	default:
		return strconv.Itoa(n)
	}
}

func (g *generator) hasImplementationGaps() bool {
	for _, object := range g.r.Report.Objects {
		for _, field := range object.Fields {
			if blocked(field.Classification) && !object.bindingFields[strings.ToLower(field.Path)] {
				return true
			}
		}
	}
	for _, check := range g.r.Report.Checks {
		if check.Blocking {
			return true
		}
	}
	return false
}

func (g *generator) serviceAvailable(name string) bool {
	_, ok := g.pools[name]
	return ok || name == serviceNoop || g.discoveredService(name)
}
