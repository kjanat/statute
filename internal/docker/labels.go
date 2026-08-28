package docker

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
)

// Label namespaces and well-known keys.
const (
	statutePrefix = "statute."
	traefikPrefix = "traefik."

	// composeServiceLabel is set by docker compose and is the natural
	// default service name for a container.
	composeServiceLabel = "com.docker.compose.service"
)

// Service is one upstream service contributed by a container: the routes
// that should reach it and the single backend this container adds to its
// pool. The provider merges Services with the same name across containers
// into one pool.
type Service struct {
	Name   string
	Routes []Matcher
	// Backend is this container's address, "ip:port" or "https://ip:port".
	Backend Backend
	// Extra holds backends folded in from other containers when
	// same-named services are merged into one pool. Extraction itself
	// never fills it.
	Extra []Backend

	// Pool-level settings, all optional label strings validated later by
	// the statute resolver. When containers in one service disagree, the
	// first container (sorted by name) wins.
	Strategy            string
	HealthCheckPath     string
	HealthCheckInterval string
	HealthCheckTimeout  string

	// Route-level middleware hints (statute labels only).
	Timeout   string
	RateLimit string
	Compress  string
}

// Backend is one container's contribution to a service pool.
type Backend struct {
	Address string
	Weight  int
	Backup  bool
}

// ExtractOptions steer label extraction.
type ExtractOptions struct {
	// Network is the preferred docker network to take container IPs from.
	Network string
	// ExposedByDefault registers containers that carry no enable label.
	ExposedByDefault bool
	// TraefikLabels also honors traefik.* labels.
	TraefikLabels bool
}

// Extract derives the services one container registers. No relevant labels
// (and ExposedByDefault off) yields no services.
//
// The second result is the refusal envelope: matchers covering traffic of
// every registration whose routes were declared and then discarded. A
// discarded router used to end in the terminal 404; those requests now
// reach Config.Fallback unless the envelope stops them. A container that
// declared no routing at all contributes none.
//
// Warnings describe labels that were understood but could not be applied.
// They are stable strings suitable for deduplicated logging.
func Extract(c Container, opts ExtractOptions) ([]Service, []Matcher, []string) {
	var svcs []Service
	var tombs []Matcher
	var warns []string

	native, nt, nw := extractNative(c, opts)
	svcs = append(svcs, native...)
	tombs = append(tombs, nt...)
	warns = append(warns, nw...)

	if opts.TraefikLabels {
		tfk, tt, tw := extractTraefik(c, opts)
		svcs = append(svcs, tfk...)
		tombs = append(tombs, tt...)
		warns = append(warns, tw...)
	}
	return svcs, tombs, warns
}

// describeEnvelope renders a refusal envelope for a log line, so an
// operator can trace a 404 back to the label that caused it.
func describeEnvelope(env []Matcher) string {
	parts := make([]string, 0, len(env))
	for _, m := range env {
		host := m.Host
		if host == "" {
			host = "any-host"
		}
		parts = append(parts, host+m.Path)
	}
	return strings.Join(parts, ", ")
}

// RefusalWarning announces that dropped routes now refuse. A global
// envelope is called out by name: it disables the operator's fallback for
// every request in the generation, which is an operational event.
func RefusalWarning(subject string, env []Matcher) string {
	if len(env) == 1 && env[0].Host == "" && env[0].Path == "/*" {
		return fmt.Sprintf("%s: routes dropped and could not be bounded to a host or path, so every unmatched request is now refused with 404 and Fallback is not consulted", subject)
	}
	return fmt.Sprintf("%s: routes dropped, refusing %s (fail-closed; these requests do not reach the fallback)", subject, describeEnvelope(env))
}

// hasPrefixedLabels reports whether any label carries the given prefix.
func hasPrefixedLabels(labels map[string]string, prefix string) bool {
	for k := range labels {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// parseBoolLabel parses a boolean label the way Traefik does
// (strconv.ParseBool: 1/t/true/True/TRUE and friends). present is false
// when the label is absent; an unparseable value reads as
// present-and-false with a non-empty warn, and that warn is the only thing
// separating it from a value that really says false.
//
// consequence completes the warning: what an unreadable value costs is
// not the same at every call site. An optional flag carries on with the
// flag off; an unreadable enable rejects the whole registration and
// refuses its declared routes. Callers go through optionBoolLabel or
// enableBoolLabel, so each consequence has one wording.
func parseBoolLabel(c Container, labels map[string]string, key, consequence string) (value, present bool, warn string) {
	v, ok := labels[key]
	if !ok {
		return false, false, ""
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return false, true, fmt.Sprintf("container %s: invalid boolean %q for label %s, %s", c.Name, v, key, consequence)
	}
	return b, true, ""
}

// optionBoolLabel reads an optional boolean that only tunes a registration
// statute still builds, so an unreadable value costs the option and nothing
// else.
func optionBoolLabel(c Container, labels map[string]string, key string) (value bool, warn string) {
	v, _, warn := parseBoolLabel(c, labels, key, "treating as false")
	return v, warn
}

// enableBoolLabel reads an enable label. An unreadable value here is not an
// opt-out: the intent could not be read, so the registration is rejected
// and the routes it declared are refused. The refusal line that follows
// names the traffic; this one must not promise the registration continues
// with the label off.
func enableBoolLabel(c Container, key string) (value, present bool, warn string) {
	return parseBoolLabel(c, c.Labels, key, "which is not an opt-out: the registration is rejected and its declared routes are refused")
}

// nativeEnabled decides whether the statute.* schema applies: an explicit
// statute.enable wins; otherwise ExposedByDefault or the presence of any
// statute.* label opts the container in.
func nativeEnabled(c Container, opts ExtractOptions) (bool, string) {
	b, present, warn := enableBoolLabel(c, "statute.enable")
	if present {
		return b, warn
	}
	return opts.ExposedByDefault || hasPrefixedLabels(c.Labels, statutePrefix), ""
}

// traefikEnabled mirrors Traefik's own semantics: with exposedByDefault
// off, only an explicit traefik.enable=true exposes the container — router
// labels alone do not.
func traefikEnabled(c Container, opts ExtractOptions) (bool, string) {
	b, present, warn := enableBoolLabel(c, "traefik.enable")
	if present {
		return b, warn
	}
	return opts.ExposedByDefault, ""
}

// defaultServiceName is the compose service name when present, else the
// container name.
func defaultServiceName(c Container) string {
	if s := c.Labels[composeServiceLabel]; s != "" {
		return s
	}
	return c.Name
}

// containerIP picks the container IP: explicit network label, then the
// provider-level preferred network, then the only network, then the
// lexicographically first network (warned).
func containerIP(c Container, labelNetwork string, opts ExtractOptions) (string, string) {
	if labelNetwork != "" {
		if ip := c.Networks[labelNetwork]; ip != "" {
			return ip, ""
		}
		return "", fmt.Sprintf("container %s: not attached to labeled network %q", c.Name, labelNetwork)
	}
	if opts.Network != "" {
		if ip := c.Networks[opts.Network]; ip != "" {
			return ip, ""
		}
	}
	if len(c.Networks) == 0 {
		return "", fmt.Sprintf("container %s: no usable network IP", c.Name)
	}
	names := make([]string, 0, len(c.Networks))
	for n := range c.Networks {
		names = append(names, n)
	}
	sort.Strings(names)
	if opts.Network != "" {
		return c.Networks[names[0]], fmt.Sprintf("container %s: not attached to network %q, using %q", c.Name, opts.Network, names[0])
	}
	if len(names) > 1 {
		return c.Networks[names[0]], fmt.Sprintf("container %s: multiple networks, using %q (set statute.network or Docker().Network to pin)", c.Name, names[0])
	}
	return c.Networks[names[0]], ""
}

// containerPort resolves the container-side port: explicit label value,
// else the lowest-numbered exposed TCP port (Traefik's rule), warning when
// that pick was ambiguous.
func containerPort(c Container, labelPort string) (int, string) {
	if labelPort != "" {
		p, err := strconv.Atoi(labelPort)
		if err != nil || p <= 0 || p > 65535 {
			return 0, fmt.Sprintf("container %s: invalid port label %q", c.Name, labelPort)
		}
		return p, ""
	}
	switch len(c.Ports) {
	case 1:
		return c.Ports[0], ""
	case 0:
		return 0, fmt.Sprintf("container %s: no exposed port and no port label", c.Name)
	default:
		// Ports is sorted ascending, so [0] is the lowest.
		return c.Ports[0], fmt.Sprintf("container %s: multiple exposed ports %v, using %d (set a port label to pick another)", c.Name, c.Ports, c.Ports[0])
	}
}

// backendAddress joins scheme, ip, and port into a resolvable address. The
// scheme is case-folded; an unrecognised value (including h2c, which
// statute's proxy does not speak) returns an empty address and a warning
// so the service is skipped rather than registered with the wrong
// protocol.
func backendAddress(c Container, scheme, ip string, port int) (string, string) {
	addr := net.JoinHostPort(ip, strconv.Itoa(port))
	switch strings.ToLower(strings.TrimSpace(scheme)) {
	case "https":
		return "https://" + addr, ""
	case "", "http":
		return addr, ""
	default:
		return "", fmt.Sprintf("container %s: unsupported backend scheme %q (statute proxies http and https only), skipping", c.Name, scheme)
	}
}

// --- native statute.* labels ---

// extractNative reads the statute label schema:
//
//	statute.enable=true|false
//	statute.service=<pool name>          (default: compose service or container name)
//	statute.port=<container port>        (default: the single exposed port)
//	statute.scheme=http|https            (default http)
//	statute.network=<docker network>
//	statute.host=<h1>[,<h2>...]          (optional; each host becomes a route)
//	statute.path=<pattern>               (default "/*")
//	statute.routes.<name>.host / .path   (additional named routes)
//	statute.weight=<n>  statute.backup=true
//	statute.strategy=round_robin|least_connections|ip_hash|weighted
//	statute.healthcheck.path/.interval/.timeout
//	statute.timeout=30s  statute.ratelimit=100/min  statute.compress=gzip,br
func extractNative(c Container, opts ExtractOptions) ([]Service, []Matcher, []string) {
	labels := c.Labels
	on, enableWarn := nativeEnabled(c, opts)
	if !on {
		// An unreadable enable is not an opt-out: enableBoolLabel warns
		// only when ParseBool failed, and the envelope covers the routes.
		if enableWarn == "" {
			return nil, nil, nil
		}
		tombs, warns := refuseNative(c, labels, []string{enableWarn})
		return nil, tombs, warns
	}
	if !nativeApplies(labels, opts) {
		return nil, nil, nil
	}

	backend, warns := nativeBackend(c, labels, opts)
	if backend == nil {
		tombs, refused := refuseNative(c, labels, warns)
		return nil, tombs, refused
	}

	routes, rw := nativeRoutes(c, labels)
	warns = append(warns, rw...)

	svc := Service{
		Name:                defaultServiceName(c),
		Routes:              routes,
		Backend:             *backend,
		Strategy:            labels["statute.strategy"],
		HealthCheckPath:     labels["statute.healthcheck.path"],
		HealthCheckInterval: labels["statute.healthcheck.interval"],
		HealthCheckTimeout:  labels["statute.healthcheck.timeout"],
		Timeout:             labels["statute.timeout"],
		RateLimit:           labels["statute.ratelimit"],
		Compress:            labels["statute.compress"],
	}
	if s := labels["statute.service"]; s != "" {
		svc.Name = s
	}
	return []Service{svc}, nil, warns
}

// refuseNative drops a container's native registration, returning its
// refusal envelope and the warnings that explain it.
func refuseNative(c Container, labels map[string]string, warns []string) ([]Matcher, []string) {
	tombs := nativeTombstones(c, labels)
	if len(tombs) > 0 {
		warns = append(warns, RefusalWarning("container "+c.Name, tombs))
	}
	return tombs, warns
}

// nativeTombstones is the refusal envelope for a container whose statute
// labels declared routes that cannot be served.
//
// The one exclusion is a container carrying no statute.* label at all:
// ExposedByDefault registered it, so its any-host "/*" matcher is
// statute's own inference, and refusing on it would disable the fallback
// for every request in the generation.
// The test is the label prefix, because a container that opted in with
// statute.enable and named neither still compiles to that same catch-all:
// it terminates every request it is given, and dropping it silently hands
// all of that traffic to Config.Fallback. That is the widest under-refusal
// the tier can have, and the envelope for it is the matcher the route had.
func nativeTombstones(c Container, labels map[string]string) []Matcher {
	if !hasPrefixedLabels(labels, statutePrefix) {
		return nil
	}
	routes, _ := nativeRoutes(c, labels)
	return EnvelopeOf(routes)
}

// nativeApplies reports whether the statute.* schema reads this container.
// Without statute labels only ExposedByDefault registers it, and never when
// traefik compat is on and traefik labels are present: that extractor
// handles those containers exclusively.
func nativeApplies(labels map[string]string, opts ExtractOptions) bool {
	if hasPrefixedLabels(labels, statutePrefix) {
		return true
	}
	if !opts.ExposedByDefault {
		return false
	}
	return !opts.TraefikLabels || !hasPrefixedLabels(labels, traefikPrefix)
}

// nativeBackend resolves the container's address, weight, and backup flag
// from the statute labels. A nil backend means the container cannot be
// routed; the warnings say why.
func nativeBackend(c Container, labels map[string]string, opts ExtractOptions) (*Backend, []string) {
	var warns []string
	ip, warn := containerIP(c, labels["statute.network"], opts)
	if warn != "" {
		warns = append(warns, warn)
	}
	if ip == "" {
		return nil, warns
	}
	port, warn := containerPort(c, labels["statute.port"])
	if warn != "" {
		warns = append(warns, warn)
	}
	if port == 0 {
		return nil, warns
	}

	weight, weightWarn := nativeWeight(c, labels)
	if weightWarn != "" {
		warns = append(warns, weightWarn)
	}
	backup, backupWarn := optionBoolLabel(c, labels, "statute.backup")
	if backupWarn != "" {
		warns = append(warns, backupWarn)
	}
	addr, schemeWarn := backendAddress(c, labels["statute.scheme"], ip, port)
	if schemeWarn != "" {
		warns = append(warns, schemeWarn)
	}
	if addr == "" {
		return nil, warns
	}
	return &Backend{
		Address: addr,
		Weight:  weight,
		Backup:  backup,
	}, warns
}

// nativeWeight parses the statute.weight label, warning and defaulting to
// 1 on invalid values.
func nativeWeight(c Container, labels map[string]string) (int, string) {
	w := labels["statute.weight"]
	if w == "" {
		return 1, ""
	}
	n, err := strconv.Atoi(w)
	if err != nil || n < 1 {
		return 1, fmt.Sprintf("container %s: invalid statute.weight %q, using 1", c.Name, w)
	}
	return n, ""
}

// nativeRoutes builds the route matchers from statute.host / statute.path
// plus any statute.routes.<name>.* groups.
func nativeRoutes(c Container, labels map[string]string) ([]Matcher, []string) {
	path := labels["statute.path"]
	if path == "" {
		path = "/*"
	}
	// Empty fragments (a trailing comma, a lone space) are skipped: an
	// empty Host means "any host", so keeping one would silently turn a
	// host-scoped registration into a catch-all.
	var routes []Matcher
	for h := range strings.SplitSeq(labels["statute.host"], ",") {
		if h = strings.TrimSpace(h); h != "" {
			routes = append(routes, Matcher{Host: strings.ToLower(h), Path: path})
		}
	}
	if len(routes) == 0 {
		routes = append(routes, Matcher{Path: path})
	}

	named, warns := namedRoutes(c, labels)
	return append(routes, named...), warns
}

// namedRoutes collects the statute.routes.<name>.host / .path label groups
// into additional matchers, ordered by route name.
func namedRoutes(c Container, labels map[string]string) ([]Matcher, []string) {
	var warns []string
	extra := map[string]*Matcher{}
	for k, v := range labels {
		rest, ok := strings.CutPrefix(k, "statute.routes.")
		if !ok {
			continue
		}
		name, field, ok := strings.Cut(rest, ".")
		if !ok || name == "" {
			warns = append(warns, fmt.Sprintf("container %s: malformed label %q", c.Name, k))
			continue
		}
		m := extra[name]
		if m == nil {
			m = &Matcher{Path: "/*"}
			extra[name] = m
		}
		switch field {
		case "host":
			m.Host = strings.ToLower(strings.TrimSpace(v))
		case "path":
			m.Path = v
		default:
			warns = append(warns, fmt.Sprintf("container %s: unknown route field in label %q", c.Name, k))
		}
	}
	routes := make([]Matcher, 0, len(extra))
	for _, n := range sortedKeys(extra) {
		routes = append(routes, *extra[n])
	}
	return routes, warns
}

// --- traefik.* compat labels ---

// traefikRouter accumulates traefik.http.routers.<name>.* labels.
type traefikRouter struct {
	name        string
	rule        string
	service     string
	middlewares string
}

// traefikService accumulates traefik.http.services.<name>.loadbalancer.*.
type traefikService struct {
	port               string
	scheme             string
	hcPath, hcInterval string
	hcTimeout          string
}

// extractTraefik reads the supported subset of Traefik's docker provider
// labels: router rules (Host/Path/PathPrefix), the router→service binding,
// loadbalancer server port/scheme, and loadbalancer health checks.
// Recognized-but-unsupported labels produce warnings; unknown traefik
// labels are ignored the way Traefik ignores other providers' labels.
func extractTraefik(c Container, opts ExtractOptions) ([]Service, []Matcher, []string) {
	labels := c.Labels
	if !hasPrefixedLabels(labels, traefikPrefix) {
		return nil, nil, nil
	}
	on, enableWarn := traefikEnabled(c, opts)
	if !on {
		// Not selected is not rejected, so an explicit opt-out leaves no
		// envelope; an unreadable value is a rejection. See extractNative.
		if enableWarn == "" {
			return nil, nil, nil
		}
		tombs, tw := traefikTombstones(c)
		return nil, tombs, append([]string{enableWarn}, tw...)
	}

	routers, services, warns := collectTraefikLabels(c)

	// An unusable container IP does not skip the router walk: the rules are
	// still parseable, and their envelopes must stay that narrow.
	ip, warn := containerIP(c, labels["traefik.docker.network"], opts)
	if warn != "" {
		warns = append(warns, warn)
	}

	// Deterministic ordering for service defaulting and output.
	routerNames := sortedKeys(routers)
	serviceNames := sortedKeys(services)

	out := map[string]*Service{}
	var tombs []Matcher
	for _, rn := range routerNames {
		r := routers[rn]
		svc, env, w := bindTraefikRouter(c, r, services, serviceNames, ip)
		warns = append(warns, w...)
		tombs = append(tombs, env...)
		if svc == nil {
			continue
		}
		if existing, ok := out[svc.Name]; ok {
			existing.Routes = append(existing.Routes, svc.Routes...)
		} else {
			out[svc.Name] = svc
		}
	}
	var list []Service
	for _, n := range sortedKeys(out) {
		list = append(list, *out[n])
	}
	return list, tombs, warns
}

// traefikTombstones is the refusal envelope for a container whose
// traefik.enable value could not be read. Nothing here may serve: the
// routers are never bound to a service. Every rule still names traffic
// that reaches Config.Fallback unless refused, so each router leaves the
// envelope of its own rule. RuleEnvelope reads it even where ParseRule
// would have succeeded: no matcher was ever built, and the envelope is a
// superset of the rule's request set by construction.
func traefikTombstones(c Container) ([]Matcher, []string) {
	routers, _, _ := collectTraefikLabels(c)
	var tombs []Matcher
	var warns []string
	for _, rn := range sortedKeys(routers) {
		env := RuleEnvelope(routers[rn].rule)
		if len(env) == 0 {
			continue
		}
		tombs = append(tombs, env...)
		warns = append(warns, RefusalWarning(fmt.Sprintf("container %s: router %q", c.Name, rn), env))
	}
	return tombs, warns
}

// collectTraefikLabels scans the label map into router and service
// accumulators, warning on recognized-but-unsupported traefik features.
func collectTraefikLabels(c Container) (map[string]*traefikRouter, map[string]*traefikService, []string) {
	routers := map[string]*traefikRouter{}
	services := map[string]*traefikService{}
	var warns []string

	for k, v := range c.Labels {
		switch {
		case strings.HasPrefix(k, "traefik.http.routers."):
			warns = append(warns, traefikRouterLabel(c, routers, k, v)...)
		case strings.HasPrefix(k, "traefik.http.services."):
			warns = append(warns, traefikServiceLabel(c, services, k, v)...)
		case strings.HasPrefix(k, "traefik.tcp.") || strings.HasPrefix(k, "traefik.udp."):
			warns = append(warns, fmt.Sprintf("container %s: traefik TCP/UDP routers are not supported (label %q)", c.Name, k))
		}
	}
	return routers, services, warns
}

// traefikRouterLabel folds one traefik.http.routers.* label into routers.
func traefikRouterLabel(c Container, routers map[string]*traefikRouter, k, v string) []string {
	rest := strings.TrimPrefix(k, "traefik.http.routers.")
	name, field, ok := strings.Cut(rest, ".")
	if !ok {
		return nil
	}
	r := routers[name]
	if r == nil {
		r = &traefikRouter{name: name}
		routers[name] = r
	}
	switch field {
	case "rule":
		r.rule = v
	case "service":
		r.service = v
	case "entrypoints", "entryPoints", "tls", "tls.certresolver", "tls.certResolver", "priority":
		// Entrypoints and TLS are listener-level concerns in statute;
		// harmless to ignore.
	case "middlewares":
		r.middlewares = v
	default:
		return []string{fmt.Sprintf("container %s: router %q: unsupported traefik router option %q ignored", c.Name, name, field)}
	}
	return nil
}

// traefikServiceLabel folds one traefik.http.services.* label into services.
func traefikServiceLabel(c Container, services map[string]*traefikService, k, v string) []string {
	rest := strings.TrimPrefix(k, "traefik.http.services.")
	name, field, ok := strings.Cut(rest, ".")
	if !ok {
		return nil
	}
	s := services[name]
	if s == nil {
		s = &traefikService{}
		services[name] = s
	}
	switch field {
	case "loadbalancer.server.port", "loadBalancer.server.port":
		s.port = v
	case "loadbalancer.server.scheme", "loadBalancer.server.scheme":
		s.scheme = v
	case "loadbalancer.healthcheck.path", "loadBalancer.healthCheck.path":
		s.hcPath = v
	case "loadbalancer.healthcheck.interval", "loadBalancer.healthCheck.interval":
		s.hcInterval = v
	case "loadbalancer.healthcheck.timeout", "loadBalancer.healthCheck.timeout":
		s.hcTimeout = v
	default:
		return []string{fmt.Sprintf("container %s: service %q: unsupported traefik service option %q ignored", c.Name, name, field)}
	}
	return nil
}

// traefikServiceName resolves the router→service binding, mirroring
// Traefik's defaulting: explicit label, else the sole service defined on
// the container, else an implicit service named after the container — so
// several label-less routers on one container share a single backend
// pool. An empty name (with warning) means the router cannot be bound.
func traefikServiceName(c Container, r *traefikRouter, serviceNames []string) (string, string) {
	if r.service != "" {
		return r.service, ""
	}
	switch len(serviceNames) {
	case 0:
		return defaultServiceName(c), ""
	case 1:
		return serviceNames[0], ""
	default:
		return "", fmt.Sprintf("container %s: router %q names no service but container defines %d, skipping", c.Name, r.name, len(serviceNames))
	}
}

// bindTraefikRouter resolves one router into a Service carrying its
// matchers and this container's backend. When the router cannot be bound it
// returns the refusal envelope covering the traffic its rule claimed.
func bindTraefikRouter(c Container, r *traefikRouter, services map[string]*traefikService, serviceNames []string, ip string) (*Service, []Matcher, []string) {
	var warns []string
	refuse := func(env []Matcher) (*Service, []Matcher, []string) {
		return nil, env, append(warns, RefusalWarning(fmt.Sprintf("container %s: router %q", c.Name, r.name), env))
	}
	// A router with no rule declares no match condition. Trim here:
	// Traefik's check is on the raw value.
	if strings.TrimSpace(r.rule) == "" {
		warns = append(warns, fmt.Sprintf("container %s: router %q has no rule, skipping", c.Name, r.name))
		return nil, nil, warns
	}
	matchers, err := ParseRule(r.rule)
	if err != nil {
		warns = append(warns, fmt.Sprintf("container %s: router %q: %v, dropping its routes", c.Name, r.name, err))
		return refuse(RuleEnvelope(r.rule))
	}
	if ip == "" {
		return refuse(EnvelopeOf(matchers))
	}
	stampMiddlewares(matchers, r.middlewares)

	svcName, warn := traefikServiceName(c, r, serviceNames)
	if warn != "" {
		warns = append(warns, warn)
	}
	if svcName == "" {
		return refuse(EnvelopeOf(matchers))
	}
	ts := services[svcName]
	if ts == nil {
		ts = &traefikService{}
	}

	addr, bw := traefikBackend(c, ts, ip)
	warns = append(warns, bw...)
	if addr == "" {
		return refuse(EnvelopeOf(matchers))
	}

	// Namespace traefik-defined pools so they cannot collide with pools
	// from native statute labels on sibling containers.
	svc := &Service{
		Name:    svcName + "@traefik",
		Routes:  matchers,
		Backend: Backend{Address: addr, Weight: 1},

		HealthCheckPath:     ts.hcPath,
		HealthCheckInterval: ts.hcInterval,
		HealthCheckTimeout:  ts.hcTimeout,
	}
	return svc, nil, warns
}

// traefikBackend resolves the service's backend address for this container.
// An empty address means the service cannot be built; the warnings say why.
func traefikBackend(c Container, ts *traefikService, ip string) (string, []string) {
	var warns []string
	port, warn := containerPort(c, ts.port)
	if warn != "" {
		warns = append(warns, warn)
	}
	if port == 0 {
		return "", warns
	}
	addr, schemeWarn := backendAddress(c, ts.scheme, ip, port)
	if schemeWarn != "" {
		warns = append(warns, schemeWarn)
	}
	return addr, warns
}

// stampMiddlewares copies a router's middleware references onto each
// matcher its rule expanded to. References are router-scoped, as in
// Traefik: they ride on the router's matchers, not the service, so
// routers sharing a pool keep their own chains.
func stampMiddlewares(matchers []Matcher, middlewares string) {
	names := splitMiddlewareNames(middlewares)
	if names == nil {
		return
	}
	for i := range matchers {
		matchers[i].Middlewares = names
	}
}

// splitMiddlewareNames parses a comma-separated middlewares label value
// into names, preserving label order and dropping empty fragments.
func splitMiddlewareNames(v string) []string {
	var names []string
	for n := range strings.SplitSeq(v, ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	return names
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
