package statute

// Upstreams maps an upstream name to its pool definition. Routes refer to
// upstreams by name.
type Upstreams map[string]Pool

// Pool is the surface upstream pool definition.
type Pool struct {
	Backends    []Backend
	Strategy    Strategy
	HealthCheck HealthCheck
	Transport   Transport
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
		return "unknown"
	}
}

// HealthCheck configures active health checks against backends.
type HealthCheck struct {
	Path      string // HTTP path to probe; empty disables active health checks
	Interval  string // how often to probe; e.g. "10s"
	Timeout   string // probe timeout; e.g. "2s"
	Healthy   int    // consecutive successes to mark healthy; defaults to 2
	Unhealthy int    // consecutive failures to mark unhealthy; defaults to 3
}

// Transport tunes the HTTP transport used to reach backends.
type Transport struct {
	MaxIdleConnsPerHost int
	IdleConnTimeout     string // e.g. "90s"
	DialTimeout         string // e.g. "5s"
	TLSHandshakeTimeout string // e.g. "5s"
}
