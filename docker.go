package statute

import (
	"statute.kjanat.dev/resolved"
)

// DockerConfig declares the Docker label-discovery provider. Construct via
// Docker.
//
// The provider is statute's one deliberately dynamic corner: the fact that
// discovery happens — the socket, the network, the opt-in policy — is still
// declared in code and validated at startup, but the routes and upstream
// pools it produces come from container labels and follow containers as
// they start and stop. Containers are discovered over the Docker Engine
// API, and label-derived routes are matched after all static routes, so
// nothing a label says can shadow the compiled configuration.
//
// Two label schemas are honored:
//
// The native schema, statute.*:
//
//	statute.enable=true
//	statute.host=app.example.com        # optional; comma-separated
//	statute.path=/api/*                 # optional; defaults to /*
//	statute.port=8080                   # optional when one port is exposed
//	statute.service=api                 # pool name; replicas sharing it pool together
//	statute.weight=2  statute.backup=true
//	statute.strategy=least_connections
//	statute.healthcheck.path=/healthz  statute.healthcheck.interval=10s
//	statute.timeout=30s  statute.ratelimit=100/min  statute.compress=gzip,br
//	statute.routes.<name>.host / .path  # additional routes
//	statute.network=proxy               # pin the docker network for the IP
//
// And, when TraefikLabels is enabled, the common subset of Traefik's
// docker labels — so containers already labeled for Traefik keep working
// unmodified:
//
//	traefik.enable=true
//	traefik.http.routers.<r>.rule=Host(`app.example.com`) && PathPrefix(`/api`)
//	traefik.http.routers.<r>.service=api
//	traefik.http.routers.<r>.middlewares=edge-security@file  # names registered via Middleware
//	traefik.http.services.<s>.loadbalancer.server.port=8080
//	traefik.http.services.<s>.loadbalancer.server.scheme=https
//	traefik.http.services.<s>.loadbalancer.healthcheck.path / .interval / .timeout
//	traefik.docker.network=proxy
//
// Rules support Host, Path, and PathPrefix combined with '&&', '||', and
// parentheses. Routers using unsupported matchers (HostRegexp, Header,
// Query, negation, …) are skipped with a logged warning rather than
// mis-routed. Traefik middleware references resolve against the code-owned
// registry declared with Middleware, scoped to their router; a router
// referencing an unregistered name is omitted with a warning rather than
// served without the middleware it asked for. PoolPolicy attaches code-owned
// transport, Host, and health policy to one exact discovered-service identity;
// labels cannot define or widen that policy.
type DockerConfig struct {
	endpoint          string
	network           string
	exposedByDefault  bool
	traefikLabels     bool
	refresh           string
	middleware        map[string][]Middleware
	defaultMiddleware []Middleware
	poolPolicy        map[string]PoolPolicy
	workloads         map[string]WorkloadPolicy
}

// Docker begins a Docker provider declaration with the default endpoint
// unix:///var/run/docker.sock.
func Docker() *DockerConfig {
	return &DockerConfig{}
}

// Endpoint sets the Docker Engine API endpoint. Accepts
// unix:///path/to/docker.sock or tcp://host:port.
func (d *DockerConfig) Endpoint(endpoint string) *DockerConfig {
	d.endpoint = endpoint
	return d
}

// Network pins the docker network whose IP is used to reach containers.
// Containers attached to several networks otherwise use the
// lexicographically first one. A per-container statute.network or
// traefik.docker.network label overrides this.
func (d *DockerConfig) Network(name string) *DockerConfig {
	d.network = name
	return d
}

// ExposedByDefault registers every running container without requiring a
// statute.enable / traefik.enable label, matching Traefik's
// exposedByDefault=true behaviour. statute's default is opt-in: only
// containers labeled enable=true (or carrying provider labels) register.
func (d *DockerConfig) ExposedByDefault() *DockerConfig {
	d.exposedByDefault = true
	return d
}

// TraefikLabels additionally honors traefik.* container labels, so a fleet
// already labeled for Traefik migrates without touching its compose files.
func (d *DockerConfig) TraefikLabels() *DockerConfig {
	d.traefikLabels = true
	return d
}

// Refresh adds a periodic full re-list of containers on top of the event
// stream, e.g. "30s". Zero (the default) relies on Docker events alone;
// the provider already re-lists whenever the event stream reconnects.
func (d *DockerConfig) Refresh(interval string) *DockerConfig {
	d.refresh = interval
	return d
}

// Middleware registers a named, code-owned middleware chain that container
// labels may reference — a traefik.http.routers.<r>.middlewares entry
// naming it verbatim attaches the chain to that router's routes, and only
// those. The mapping keeps the config-as-code trust boundary: labels can
// only select policies compiled into the binary, never define new ones. A
// router referencing an unregistered name fails closed — its routes are
// omitted with a warning. Registering the same name again replaces the
// earlier chain.
func (d *DockerConfig) Middleware(name string, mws ...Middleware) *DockerConfig {
	if d.middleware == nil {
		d.middleware = map[string][]Middleware{}
	}
	d.middleware[name] = mws
	return d
}

// DefaultMiddleware appends middleware applied to every Docker-discovered
// route, outermost — before any label-referenced chains and the
// statute.timeout / statute.ratelimit / statute.compress hints.
func (d *DockerConfig) DefaultMiddleware(mws ...Middleware) *DockerConfig {
	d.defaultMiddleware = append(d.defaultMiddleware, mws...)
	return d
}

// PoolPolicy registers code-owned pool policy for one discovered-service
// identity, such as "foo@traefik". Docker continues to own the service's
// backends, strategy, and routes. The exact-name mapping prevents policy from
// leaking to another service or into router-scoped middleware; an unmatched
// name produces a Docker provider diagnostic. Registering the same name again
// replaces the earlier policy.
func (d *DockerConfig) PoolPolicy(name string, policy PoolPolicy) *DockerConfig {
	if d.poolPolicy == nil {
		d.poolPolicy = map[string]PoolPolicy{}
	}
	d.poolPolicy[name] = policy
	return d
}

// Workload registers code-owned on-demand activation policy for one
// discovered-service identity, such as "foo@traefik". The matched service's
// container starts when a routed request needs it, serves once readiness is
// established, and stops after IdleAfter. Only registration grants that
// authority; a label can never widen it. The policy requires a one-to-one
// service and container pair: a merged service has no single activation
// owner, and a container beneath several services has no single
// controllable lifecycle, since a stop acts on all of them at once. The
// provider reports when either shape bars the policy. An unmatched name
// produces a Docker provider diagnostic. Registering the same name again
// replaces the earlier policy.
func (d *DockerConfig) Workload(name string, policy WorkloadPolicy) *DockerConfig {
	if d.workloads == nil {
		d.workloads = map[string]WorkloadPolicy{}
	}
	d.workloads[name] = policy
	return d
}

// WorkloadPolicy configures on-demand activation for one Docker-discovered
// service. The zero value of every field selects its default.
type WorkloadPolicy struct {
	// IdleAfter stops the container this long after the last in-flight
	// request, WebSocket, or streaming response finished. Default "15m".
	IdleAfter string
	// StartTimeout bounds the Docker start call. Default "30s".
	StartTimeout string
	// ReadyTimeout bounds the wait for readiness after a start. Default "2m".
	ReadyTimeout string
	// BackoffBase and BackoffCap bound the exponential backoff between
	// failed activations. Defaults "5s" and "5m".
	BackoffBase string
	BackoffCap  string
	// Readiness selects how an activated container proves it can serve;
	// the zero value is automatic. See Readiness.
	Readiness Readiness
}

// Readiness is a workload readiness policy. The zero value is automatic:
// the container's HEALTHCHECK when it defines one, else a TCP connect.
// Construct the explicit policies from DockerHealthReadiness, TCPReadiness,
// or HTTPReadiness. There is no policy that trusts a running container:
// running is not ready.
type Readiness struct {
	mode resolved.ReadinessMode
	path string
}

// DockerHealthReadiness waits for the container's HEALTHCHECK to report
// healthy. A container without a HEALTHCHECK never becomes ready under it.
var DockerHealthReadiness = Readiness{mode: resolved.ReadinessDockerHealth}

// TCPReadiness waits for a TCP connect to the discovered backend to succeed.
var TCPReadiness = Readiness{mode: resolved.ReadinessTCP}

// HTTPReadiness probes the given path over the pool's transport until the
// response status is in the 200-399 range.
func HTTPReadiness(path string) Readiness {
	return Readiness{mode: resolved.ReadinessHTTP, path: path}
}

// String returns the canonical name of the readiness policy.
func (r Readiness) String() string {
	switch r.mode {
	case resolved.ReadinessAuto:
		return "auto"
	case resolved.ReadinessDockerHealth:
		return "docker_health"
	case resolved.ReadinessTCP:
		return "tcp"
	case resolved.ReadinessHTTP:
		return "http:" + r.path
	default:
		return enumUnknown
	}
}
