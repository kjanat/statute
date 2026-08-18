package statute

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
//	traefik.http.services.<s>.loadbalancer.server.port=8080
//	traefik.http.services.<s>.loadbalancer.server.scheme=https
//	traefik.http.services.<s>.loadbalancer.healthcheck.path / .interval / .timeout
//	traefik.docker.network=proxy
//
// Rules support Host, Path, and PathPrefix combined with '&&', '||', and
// parentheses. Routers using unsupported matchers (HostRegexp, Header,
// Query, negation, …) are skipped with a logged warning rather than
// mis-routed. Traefik middleware references are ignored with a warning;
// attach statute.timeout / statute.ratelimit / statute.compress labels
// instead.
type DockerConfig struct {
	endpoint         string
	network          string
	exposedByDefault bool
	traefikLabels    bool
	refresh          string
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
