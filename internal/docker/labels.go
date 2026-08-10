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

// Extract derives the services a single container registers. A container
// with no relevant labels (and ExposedByDefault off) yields no services.
// Warnings describe labels that were understood but could not be applied;
// they are stable strings suitable for deduplicated logging.
func Extract(c Container, opts ExtractOptions) ([]Service, []string) {
	var svcs []Service
	var warns []string

	native, nw := extractNative(c, opts)
	svcs = append(svcs, native...)
	warns = append(warns, nw...)

	if opts.TraefikLabels {
		tfk, tw := extractTraefik(c, opts)
		svcs = append(svcs, tfk...)
		warns = append(warns, tw...)
	}
	return svcs, warns
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

// boolLabel parses a boolean label the way Traefik does (strconv.ParseBool:
// 1/t/true/True/TRUE and friends). present is false when the label is
// absent; an unparseable value counts as present-and-false with a warning.
func boolLabel(c Container, labels map[string]string, key string) (value, present bool, warn string) {
	v, ok := labels[key]
	if !ok {
		return false, false, ""
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return false, true, fmt.Sprintf("container %s: invalid boolean %q for label %s, treating as false", c.Name, v, key)
	}
	return b, true, ""
}

// nativeEnabled decides whether the statute.* schema applies: an explicit
// statute.enable wins; otherwise ExposedByDefault or the presence of any
// statute.* label opts the container in.
func nativeEnabled(c Container, opts ExtractOptions) (bool, string) {
	b, present, warn := boolLabel(c, c.Labels, "statute.enable")
	if present {
		return b, warn
	}
	return opts.ExposedByDefault || hasPrefixedLabels(c.Labels, statutePrefix), ""
}

// traefikEnabled mirrors Traefik's own semantics: with exposedByDefault
// off, only an explicit traefik.enable=true exposes the container — router
// labels alone do not.
func traefikEnabled(c Container, opts ExtractOptions) (bool, string) {
	b, present, warn := boolLabel(c, c.Labels, "traefik.enable")
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
// scheme is case-folded; an unrecognised value warns and falls back to
// http so the typo is visible instead of silently downgrading.
func backendAddress(c Container, scheme, ip string, port int) (string, string) {
	addr := net.JoinHostPort(ip, strconv.Itoa(port))
	switch strings.ToLower(strings.TrimSpace(scheme)) {
	case "https":
		return "https://" + addr, ""
	case "", "http":
		return addr, ""
	default:
		return addr, fmt.Sprintf("container %s: unsupported scheme %q, using http", c.Name, scheme)
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
func extractNative(c Container, opts ExtractOptions) ([]Service, []string) {
	labels := c.Labels
	on, enableWarn := nativeEnabled(c, opts)
	if !on {
		if enableWarn != "" {
			return nil, []string{enableWarn}
		}
		return nil, nil
	}
	if !hasPrefixedLabels(labels, statutePrefix) && !opts.ExposedByDefault {
		return nil, nil
	}
	// When traefik compat is on and the container has traefik labels but no
	// statute ones, let the traefik extractor handle it exclusively.
	if !hasPrefixedLabels(labels, statutePrefix) && opts.TraefikLabels && hasPrefixedLabels(labels, traefikPrefix) {
		return nil, nil
	}

	backend, warns := nativeBackend(c, labels, opts)
	if backend == nil {
		return nil, warns
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
	return []Service{svc}, warns
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

	weight := 1
	if w := labels["statute.weight"]; w != "" {
		n, err := strconv.Atoi(w)
		if err != nil || n < 1 {
			warns = append(warns, fmt.Sprintf("container %s: invalid statute.weight %q, using 1", c.Name, w))
		} else {
			weight = n
		}
	}
	backup, _, backupWarn := boolLabel(c, labels, "statute.backup")
	if backupWarn != "" {
		warns = append(warns, backupWarn)
	}
	addr, schemeWarn := backendAddress(c, labels["statute.scheme"], ip, port)
	if schemeWarn != "" {
		warns = append(warns, schemeWarn)
	}
	return &Backend{
		Address: addr,
		Weight:  weight,
		Backup:  backup,
	}, warns
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
	name    string
	rule    string
	service string
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
func extractTraefik(c Container, opts ExtractOptions) ([]Service, []string) {
	labels := c.Labels
	if !hasPrefixedLabels(labels, traefikPrefix) {
		return nil, nil
	}
	on, enableWarn := traefikEnabled(c, opts)
	if !on {
		if enableWarn != "" {
			return nil, []string{enableWarn}
		}
		return nil, nil
	}

	routers, services, warns := collectTraefikLabels(c)

	ip, warn := containerIP(c, labels["traefik.docker.network"], opts)
	if warn != "" {
		warns = append(warns, warn)
	}
	if ip == "" {
		return nil, warns
	}

	// Deterministic ordering for service defaulting and output.
	routerNames := sortedKeys(routers)
	serviceNames := sortedKeys(services)

	out := map[string]*Service{}
	for _, rn := range routerNames {
		r := routers[rn]
		svc, w := bindTraefikRouter(c, r, services, serviceNames, ip, opts)
		warns = append(warns, w...)
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
	return list, warns
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
		return []string{fmt.Sprintf("container %s: router %q: traefik middlewares are not supported, ignoring %q (use statute.timeout / statute.ratelimit / statute.compress labels)", c.Name, name, v)}
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

// bindTraefikRouter resolves one router into a Service carrying its
// matchers and this container's backend.
func bindTraefikRouter(c Container, r *traefikRouter, services map[string]*traefikService, serviceNames []string, ip string, _ ExtractOptions) (*Service, []string) {
	var warns []string
	if r.rule == "" {
		warns = append(warns, fmt.Sprintf("container %s: router %q has no rule, skipping", c.Name, r.name))
		return nil, warns
	}
	matchers, err := ParseRule(r.rule)
	if err != nil {
		warns = append(warns, fmt.Sprintf("container %s: router %q: %v, skipping", c.Name, r.name, err))
		return nil, warns
	}

	// Router→service binding, mirroring Traefik's defaulting: explicit
	// label, else the sole service defined on the container, else an
	// implicit service named after the container — so several label-less
	// routers on one container share a single backend pool, as in Traefik.
	svcName := r.service
	if svcName == "" {
		if len(serviceNames) == 1 {
			svcName = serviceNames[0]
		} else if len(serviceNames) > 1 {
			warns = append(warns, fmt.Sprintf("container %s: router %q names no service but container defines %d, skipping", c.Name, r.name, len(serviceNames)))
			return nil, warns
		} else {
			svcName = defaultServiceName(c)
		}
	}
	ts := services[svcName]
	if ts == nil {
		ts = &traefikService{}
	}

	port, warn := containerPort(c, ts.port)
	if warn != "" {
		warns = append(warns, warn)
	}
	if port == 0 {
		return nil, warns
	}
	addr, schemeWarn := backendAddress(c, ts.scheme, ip, port)
	if schemeWarn != "" {
		warns = append(warns, schemeWarn)
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
	return svc, warns
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
