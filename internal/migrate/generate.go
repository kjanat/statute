package migrate

import (
	"errors"
	"fmt"
	"go/format"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	statute "statute.kjanat.dev"
	"statute.kjanat.dev/internal/docker"
)

type middleware struct {
	values []statute.Middleware
	code   []string
}

type generator struct {
	r              *Result
	listeners      []string
	pools          map[string]string
	routes         []string
	fallbacks      []string
	docker         string
	objects        map[string]*Object
	chains         map[string]middleware
	visiting       map[string]bool
	attempted      map[string]bool
	transports     map[string]statute.Transport
	transportCode  map[string]string
	hosts          map[string]bool
	certFile       string
	keyFile        string
	imports        map[string]bool
	helpers        map[string]string
	entrypoints    map[string]*Object
	entryTLS       map[string]bool
	routerEntries  map[*Object][]string
	contentEntries []string
	defaults       string
	defaultEntries []string
}

func generate(r *Result) error {
	g := &generator{r: r, pools: map[string]string{}, objects: map[string]*Object{}, chains: map[string]middleware{}, visiting: map[string]bool{}, attempted: map[string]bool{}, transports: map[string]statute.Transport{}, transportCode: map[string]string{}, hosts: map[string]bool{}}
	g.imports = map[string]bool{}
	g.helpers = map[string]string{}
	g.entrypoints = map[string]*Object{}
	g.entryTLS = map[string]bool{}
	g.routerEntries = map[*Object][]string{}
	r.Config.Upstreams = statute.Upstreams{}
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
	g.checkPrecedence(routers)
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
	case "tcp", "udp":
		o.mark("", OutOfScope, "TCP/UDP migration is outside the HTTP MVP")
		o.note("$parity", Manual, "non-HTTP traffic requires a separate migration")
	case "setting":
		if o.Name == "api" || o.Name == "dashboard" {
			o.mark("", OutOfScope, "Traefik dashboard is outside Statute's scope")
		}
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
	d := statute.Docker().TraefikLabels()
	code := "statute.Docker().TraefikLabels()" + dockerEndpoint(o, d) + dockerNetwork(o, d)
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
	pool := statute.Pool{Transport: g.transports["default@internal"]}
	backends, ok := serviceBackends(o, &pool)
	if !ok {
		return
	}
	parts := []string{"Backends:[]statute.Backend{" + strings.Join(backends, ",") + "}"}
	parts = append(parts, serviceHost(o, &pool)...)
	parts = append(parts, serviceHealth(o, &pool)...)
	if name := o.text("loadBalancer.serversTransport"); name != "" {
		o.reference("loadBalancer.serversTransport", "transport", name)
		id := qualified(name, o.Provider)
		t, found := g.transports[id]
		if !found {
			o.note("$transport", Manual, "transport reference is missing or has blocking fields")
			return
		}
		pool.Transport = t

		o.mark("loadBalancer.serversTransport", Translated, "provider-qualified code-owned pool transport")
	}
	serviceFlush(o, &pool.Transport)
	parts = append(parts, "Transport:"+transportLiteral(pool.Transport))
	if !o.ready() {
		return
	}
	id := o.Name + "@file"
	g.r.Config.Upstreams[id] = pool
	g.pools[id] = "statute.Pool{" + strings.Join(parts, ",") + "}"
	o.Generated = true
	if name := o.text("loadBalancer.serversTransport"); name != "" {
		if transport := g.objects["serversTransports:"+qualified(name, o.Provider)]; transport != nil {
			transport.Generated = true
		}
	}
}

func serviceBackends(o *Object, pool *statute.Pool) ([]string, bool) {
	servers, ok := o.value("loadBalancer.servers").([]any)
	if !ok || len(servers) == 0 {
		return nil, false
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
	for _, key := range []string{"loadbalancer.server.port", "loadbalancer.server.scheme"} {
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
	case "loadbalancer.server.port":
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
	ms, ok := g.parseRouter(o)
	if !ok {
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
	if target, status, ok := g.redirect(o, refs); ok {
		g.redirectRoutes(o, ms, target, status)
		return
	}
	name := qualified(o.text("service"), o.Provider)
	o.reference("service", "service", o.text("service"))
	if _, ok := g.pools[name]; !ok {
		o.note("$service", Manual, "service reference is missing, unsupported or blocked")
		return
	}
	o.mark("service", Translated, "provider-qualified native upstream pool")
	if !nativePaths(ms) {
		o.mark("rule", Manual, "Traefik PathPrefix is a byte prefix (/api includes /api-secret); native wildcard paths are segment-aware")
		return
	}
	if !o.ready() {
		return
	}
	for _, m := range ms {
		g.proxyRoute(m, name, chain)
	}
	g.markMiddlewaresUsed(refs, o.Provider)
	o.Generated = true
}

func (g *generator) parseRouter(o *Object) ([]docker.Matcher, bool) {
	rule := o.text("rule")
	ms, err := docker.ParseRule(rule)
	if err != nil || !literal(rule) {
		g.r.Report.UnknownRouteCounts++
		o.mark("rule", Unsupported, "unsupported or unresolved matcher; rule text withheld")
		o.note("$rule", Unsupported, "router rule cannot be translated")
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
	g.checkDefaultMiddleware(o, refs)
	g.checkMiddlewareOrder(o, refs)
	for _, ref := range refs {
		if !literal(ref) {
			o.note("$middleware", Manual, "unresolved middleware reference; interpolation is not evaluated")
			continue
		}
		o.reference("middlewares", "middleware", ref)
		id := qualified(ref, o.Provider)
		mw, found := g.chains[id]
		if !found {
			o.note("$middleware:"+id, Manual, "missing or blocked provider-qualified middleware; route must not serve")
			continue
		}
		if len(chain.values)+len(mw.values) > 256 {
			o.note("$size", Unsupported, "assembled middleware chain exceeds 256 operations")
			return chain
		}
		chain.values = append(chain.values, mw.values...)
		chain.code = append(chain.code, mw.code...)
	}
	if len(refs) > 0 && len(chain.code) > 0 {
		o.mark("middlewares", Translated, "ordered code-owned middleware chain, scoped to this router")
	}
	return chain
}

func (g *generator) dockerRouter(o *Object, count int) {
	o.reference("service", "service", o.text("service"))
	if name := o.text("service"); name != "" {
		if !strings.Contains(name, "@") {
			o.mark("service", Exact, "retained Docker service reference")
		} else {
			o.mark("service", Manual, "cross-provider Docker service references require explicit migration")
		}
	}
	if g.r.Config.Docker == nil {
		o.note("$provider", Manual, "supply static Docker provider settings before enabling discovery")
	}
	if o.value("priority") != nil {
		o.mark("priority", Manual, "Docker runtime uses matcher specificity, not Traefik numeric priority")
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
	if !service.ready() || service.value("loadbalancer.server.port") == nil {
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

func nativePaths(ms []docker.Matcher) bool {
	for _, m := range ms {
		if m.PathKind == docker.PathByte || (m.PathKind == docker.PathExact && strings.HasSuffix(m.Path, "/*")) {
			return false
		}
	}
	return true
}

func (g *generator) proxyRoute(m docker.Matcher, name string, chain middleware) {
	hosts := hostSpellings(m.Host)
	route := statute.Match(m.Path).ProxyTo(name).With(chain.values...)
	code := fmt.Sprintf("statute.Match(%q).ProxyTo(%q)", m.Path, name)
	if len(hosts) > 0 {
		route.Hosts(hosts...)
		code += ".Hosts(" + quoted(hosts) + ")"
	}
	if len(chain.code) > 0 {
		code += ".With(" + strings.Join(chain.code, ",") + ")"
	}
	if m.Host == "" && m.PathKind == docker.PathAny {
		g.r.Config.FallbackRoutes = append(g.r.Config.FallbackRoutes, route)
		g.fallbacks = append(g.fallbacks, code)
	} else {
		g.r.Config.Routes = append(g.r.Config.Routes, route)
		g.routes = append(g.routes, code)
	}
	g.r.Report.GeneratedRoutes += max(1, len(hosts))
}

func (g *generator) redirect(o *Object, refs []string) (string, int, bool) {
	if len(refs) != 1 {
		return "", 0, false
	}
	m := g.objects["middlewares:"+qualified(refs[0], o.Provider)]
	if m == nil {
		return "", 0, false
	}
	scheme := m.text("redirectScheme.scheme")
	if scheme != protocolHTTPS && scheme != protocolHTTP {
		return "", 0, false
	}
	if m.value("redirectScheme.port") != nil {
		return "", 0, false
	}
	status := 302
	if v := m.value("redirectScheme.permanent"); v != nil {
		b, err := strconv.ParseBool(scalar(v))
		if err != nil {
			return "", 0, false
		}
		if b {
			status = 301
		}
	}
	// The candidate is useful for review but scheme detection/trusted forwarded
	// headers and HTTPS listeners prevent a general exact substitution.
	m.note("$redirect", Manual, "review forwarded-scheme trust and listener reachability before accepting redirect action")
	o.note("$redirect", Manual, "candidate redirect action requires scheme and listener review")
	m.Generated = true
	return scheme + "://{host}{request_uri}", status, true
}

func (g *generator) redirectRoutes(o *Object, ms []docker.Matcher, target string, status int) {
	for _, m := range ms {
		if m.PathKind == docker.PathByte || (m.PathKind == docker.PathExact && strings.HasSuffix(m.Path, "/*")) {
			return
		}
	}
	for _, m := range ms {
		r := statute.Match(m.Path).RedirectTo(target, status)
		code := fmt.Sprintf("statute.Match(%q).RedirectTo(%q,%d)", m.Path, target, status)
		if hosts := hostSpellings(m.Host); len(hosts) > 0 {
			r.Hosts(hosts...)
			code += ".Hosts(" + quoted(hosts) + ")"
			g.r.Report.GeneratedRoutes += len(hosts)
		} else {
			g.r.Report.GeneratedRoutes++
		}
		g.r.Config.Routes = append(g.r.Config.Routes, r)
		g.routes = append(g.routes, code)
	}
	o.Generated = true
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

func (g *generator) checkPrecedence(routers []*Object) {
	for i, a := range routers {
		if i != len(routers)-1 && generatedCatchAll(a) {
			a.note("$fallback", Manual, "catch-all is not last in source priority; moving it to fallback changes precedence")
		}
		for _, b := range routers[i+1:] {
			if needsPrecedenceReview(a, b) && rulesMayOverlap(a, b) {
				a.note("$precedence:"+b.Name+"@"+b.Provider, Manual, "overlapping routes require explicit priority/static-Docker precedence review")
			}
		}
	}
}

func generatedCatchAll(o *Object) bool {
	if o.Provider != providerFile || !o.Generated {
		return false
	}
	ms, _ := docker.ParseRule(o.text("rule"))
	for _, m := range ms {
		if m.Host == "" && m.PathKind == docker.PathAny {
			return true
		}
	}
	return false
}

func needsPrecedenceReview(a, b *Object) bool {
	return a.Provider != b.Provider || priority(a) == priority(b) || a.Provider == providerDocker
}

func rulesMayOverlap(a, b *Object) bool {
	x, ex := docker.ParseRule(a.text("rule"))
	y, ey := docker.ParseRule(b.text("rule"))
	if ex != nil || ey != nil {
		return true
	}
	for _, m := range x {
		for _, n := range y {
			if matchersMayOverlap(m, n) {
				return true
			}
		}
	}
	return false
}

func matchersMayOverlap(m, n docker.Matcher) bool {
	if m.PathKind == docker.PathExact && n.PathKind == docker.PathExact && m.Path != n.Path {
		return false
	}
	if m.Host == "" || n.Host == "" {
		return true
	}
	for _, a := range hostSpellings(m.Host) {
		for _, b := range hostSpellings(n.Host) {
			if strings.EqualFold(a, b) {
				return true
			}
		}
	}
	return false
}

func (g *generator) validate() {
	resolved, err := statute.Resolve(g.r.Config)
	if err != nil {
		g.r.Report.Checks = append(g.r.Report.Checks, Check{toolResolve, "invalid-generated-config", true, false})
		for _, tool := range []string{toolLint, toolExport, toolGraph, toolTopology} {
			g.r.Report.Checks = append(g.r.Report.Checks, Check{Tool: tool, Code: "not-run-invalid-generated-config", Blocking: true})
		}
		return
	}
	g.r.Report.Checks = append(g.r.Report.Checks, Check{toolResolve, "ok", false, false})
	findings, err := statute.Lint(g.r.Config)
	if err != nil {
		g.r.Report.Checks = append(g.r.Report.Checks, Check{toolLint, "failed", true, false})
	}
	for _, f := range findings {
		g.r.Report.Checks = append(g.r.Report.Checks, Check{toolLint, f.Code, f.Severity == statute.SeverityError, false})
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
		g.r.Report.Checks = append(g.r.Report.Checks, Check{check.name, code, failed, false})
	}
	g.checkTopology(len(resolved.Routes)+len(resolved.FallbackRoutes), len(resolved.Upstreams))
	g.checkSourceTopology(resolved)
	g.checkDockerTopology(resolved)
	g.checkDockerExtraction(resolved)
}

func (g *generator) checkTopology(routes, pools int) {
	want := 0
	for _, o := range g.r.Report.Objects {
		if o.Kind != "routers" || o.Provider != providerFile || !o.Generated {
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
	failed := routes != want || pools != len(g.pools)
	code := "generated-route-and-pool-counts-match"
	if failed {
		code = "generated-topology-mismatch"
	}
	g.r.Report.Checks = append(g.r.Report.Checks, Check{toolTopology, code, failed, false})
}

func (g *generator) source() ([]byte, error) {
	var b strings.Builder
	b.WriteString("// Generated migration configuration. Review the accompanying audit before deployment.\npackage main\n\nimport statute \"statute.kjanat.dev\"\n")
	for _, name := range keys(g.imports) {
		fmt.Fprintf(&b, "import %q\n", name)
	}
	for _, name := range keys(g.helpers) {
		b.WriteString(g.helpers[name])
		b.WriteString("\n")
	}
	for _, o := range g.r.Report.Objects {
		for _, f := range o.Fields {
			if blocked(f.Classification) {
				fmt.Fprintf(&b, "// TODO %q %q: %s\n", o.identity(), f.Path, f.Reason)
			}
		}
	}
	b.WriteString("\nfunc configuration() statute.Config {\nreturn statute.Config{\n")
	b.WriteString("Defaults:" + g.defaults + ",\n")
	b.WriteString("Listeners:statute.Listeners{" + strings.Join(g.listeners, ",") + "},\nUpstreams:statute.Upstreams{\n")
	for _, name := range keys(g.pools) {
		fmt.Fprintf(&b, "%q:%s,\n", name, g.pools[name])
	}
	b.WriteString("},\nRoutes:statute.Routes{" + strings.Join(g.routes, ",") + "},\nFallbackRoutes:statute.Routes{" + strings.Join(g.fallbacks, ",") + "},\n")
	if g.docker != "" {
		b.WriteString("Docker:" + g.docker + ",\n")
	}
	b.WriteString("}\n}\n\nfunc main(){\n")
	if g.r.Report.Blocking {
		b.WriteString("// TODO: resolve every blocking audit finding and validate the completed configuration.\npanic(\"migration incomplete: resolve blocking findings before serving\")\n")
	} else {
		b.WriteString("statute.Main(configuration())\n")
	}
	b.WriteString("}\n")
	return format.Source([]byte(b.String()))
}
