package statute

// Upstreams maps an upstream name to its pool definition. Routes refer to
// upstreams by name.
type Upstreams map[string]Pool

// Pool is the surface upstream pool definition.
type Pool struct {
	Backends    []Backend
	Strategy    Strategy
	HealthCheck HealthCheck
	// PassiveHealthCheck demotes backends based on real proxy traffic; it
	// works with or without an active HealthCheck.
	PassiveHealthCheck PassiveHealthCheck
	Transport          Transport
	// UpstreamHost selects the Host header backends receive. The zero value
	// is ClientHost, today's behavior: the client's own Host is forwarded.
	UpstreamHost UpstreamHost
}

// Backend is a single upstream target.
type Backend struct {
	// Address is the host:port of the backend.
	Address string
	// Weight is the relative weight for weighted strategies. Defaults to 1.
	Weight int
	// Backup is true for failover-only backends; they receive traffic only
	// when all primary backends are unhealthy.
	Backup bool
}

// UpstreamHost is a pool's outgoing Host header policy. The zero value
// forwards the client's Host unchanged; construct the other policies from
// TargetHost or HostValue. Hostname-sensitive backends that reject the
// client's Host usually want TargetHost.
//
// The policy applies to proxied requests and to active health-check probes
// alike, so a backend that routes on Host sees consistent traffic. A probe
// has no client, so under ClientHost it carries the backend's own host.
// HealthCheck.Host, when set, overrides the probe half only; proxied
// requests keep following this policy.
type UpstreamHost struct {
	mode  hostMode
	value string
}

type hostMode int

const (
	hostModeClient hostMode = iota
	hostModeTarget
	hostModeExplicit
)

// ClientHost forwards the client's original Host header to the backend.
// This is the default.
var ClientHost = UpstreamHost{mode: hostModeClient}

// TargetHost sends each backend its own host, taken from its address —
// what a plain http.Client dialing the backend directly would send.
var TargetHost = UpstreamHost{mode: hostModeTarget}

// HostValue sends the given fixed Host header to every backend in the pool.
func HostValue(host string) UpstreamHost {
	return UpstreamHost{mode: hostModeExplicit, value: host}
}

// String returns the canonical name of the policy.
func (u UpstreamHost) String() string {
	switch u.mode {
	case hostModeClient:
		return "client_host"
	case hostModeTarget:
		return "target_host"
	case hostModeExplicit:
		return "host:" + u.value
	default:
		return enumUnknown
	}
}

// Strategy selects how a request is routed across the backends in a pool.
type Strategy int

const (
	// RoundRobin distributes requests evenly across backends in declaration order.
	RoundRobin Strategy = iota
	// LeastConnections sends each request to the backend with the fewest in-flight requests.
	LeastConnections
	// IPHash routes requests from the same client IP to the same backend (consistent hash).
	IPHash
	// Weighted distributes requests proportionally to each backend's Weight.
	Weighted
)

// String returns the canonical name of the strategy.
func (s Strategy) String() string {
	switch s {
	case RoundRobin:
		return "round_robin"
	case LeastConnections:
		return "least_connections"
	case IPHash:
		return "ip_hash"
	case Weighted:
		return "weighted"
	default:
		return enumUnknown
	}
}

// HealthCheck configures active health checks against backends.
type HealthCheck struct {
	Path      string // HTTP path to probe; empty disables active health checks
	Interval  string // how often to probe; e.g. "10s"
	Timeout   string // probe timeout; e.g. "2s"
	Healthy   int    // consecutive successes to mark healthy; defaults to 2
	Unhealthy int    // consecutive failures to mark unhealthy; defaults to 3

	// Host overrides the Host header probes carry. Empty derives the probe
	// host from the pool's UpstreamHost policy as before: an explicit
	// HostValue rides every probe, and the other policies leave probes on
	// each backend's own host. UpstreamHost governs proxied requests either
	// way. The value is validated like HostValue's.
	Host string
	// Statuses are the probe response statuses accepted as healthy; empty
	// keeps the default 200-399 range. Each entry must be within 100-599.
	Statuses []int
}

// PassiveHealthCheck configures passive backend demotion from proxy
// outcomes: a backend that accumulates MaxFailures failed attempts — a
// transport error or a 5xx response — inside the sliding FailureWindow is
// excluded from selection. A request its own client canceled is not a
// failure; a deadline that expired waiting on the backend is. Failures
// count per backend attempt, so under
// Retry each attempt counts against the backend that served it. A success
// does not clear the window; a backend is re-admitted only as failures age
// out. A pool whose every backend is demoted keeps serving in degraded
// mode. Both fields must be set together; the zero value disables passive
// health checks.
type PassiveHealthCheck struct {
	FailureWindow string // sliding window; e.g. "30s"
	MaxFailures   int    // failures within the window that demote
}

// Transport tunes the HTTP transport used to reach backends. The TLS fields
// apply to backends dialed over https and set one verification policy for the
// pool: reverse-proxy requests and active health-check probes share it.
type Transport struct {
	MaxIdleConnsPerHost int
	IdleConnTimeout     string // e.g. "90s"
	DialTimeout         string // e.g. "5s"
	TLSHandshakeTimeout string // e.g. "5s"

	// ServerName overrides the hostname verified against the backend's
	// certificate (and sent as SNI). Set it when backends are dialed by IP
	// but present a certificate for a DNS name.
	ServerName string
	// RootCAFiles are PEM files whose certificates replace the system roots
	// when verifying backend certificates. Useful for internal CAs.
	RootCAFiles []string
	// InsecureSkipVerify disables backend certificate verification
	// entirely. The lint rule TLS002 warns when it is set; prefer
	// RootCAFiles and ServerName.
	InsecureSkipVerify bool
}
