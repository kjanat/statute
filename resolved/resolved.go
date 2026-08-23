// Package resolved is the canonical, fully-validated schema that the statute
// runtime operates on. It is produced by statute.Resolve from a surface Config.
//
// All durations are time.Duration. All upstream references are pointers. All
// optional fields have been filled with their resolved defaults. There are no
// string-encoded values.
//
// Tooling (validators, dashboards, doc generators) should target this package.
// End-user configurations should not — they should use the surface API in the
// statute package.
package resolved

import (
	"io"
	"time"
)

// Config is the resolved top-level configuration.
type Config struct {
	Listeners     []*Listener
	Upstreams     map[string]*Pool
	Routes        []*Route
	Docker        *Docker // nil unless Docker label discovery is enabled
	Defaults      Defaults
	Observability Observability
	Shutdown      Shutdown
}

// Docker is the resolved Docker label-discovery provider configuration.
// The provider's output (label-derived routes and pools) is runtime state,
// not part of the resolved schema; only the discovery settings are.
type Docker struct {
	Endpoint         string
	Network          string
	ExposedByDefault bool
	TraefikLabels    bool
	// Refresh is the periodic full-resync interval; zero means events only.
	Refresh time.Duration
}

// Listener is a resolved listener.
type Listener struct {
	Addr     string
	Scheme   string // "http" or "https"
	Redirect string // non-empty: this listener is a redirect-only listener

	AutoTLS     *AutoTLS   // nil unless this is an HTTPS listener with ACME
	StaticTLS   *StaticTLS // nil unless this is an HTTPS listener with static certs
	EnableHTTP2 bool
	HTTP3Addr   string // empty unless HTTP/3 is enabled

	// BehindCloudflare indicates the listener is fronted by Cloudflare. The
	// runtime suppresses TLS-ALPN-01 challenges (HTTP-01 only) and trusts
	// CF-Connecting-IP / True-Client-IP for client IP attribution.
	BehindCloudflare bool
}

// AutoTLS is the resolved ACME configuration.
type AutoTLS struct {
	Domains []string
	Email   string
	Storage string
	DNS01   *CloudflareDNS01
}

// CloudflareDNS01 is the resolved DNS-01 configuration. When non-nil, the
// runtime uses Cloudflare's DNS API to satisfy challenges instead of HTTP-01.
type CloudflareDNS01 struct {
	APIToken string
	ZoneID   string // optional; empty means auto-discover
}

// StaticTLS is the resolved static-cert configuration.
type StaticTLS struct {
	CertFile string
	KeyFile  string
}

// Pool is a resolved upstream pool.
type Pool struct {
	Name        string
	Backends    []Backend
	Strategy    Strategy
	HealthCheck HealthCheck
	Transport   Transport
}

// Backend is a resolved backend target.
type Backend struct {
	Address string
	Weight  int
	Backup  bool
}

// Strategy mirrors the surface Strategy enum.
type Strategy int

// Load-balancing strategies. Values match the surface API constants in the
// statute package one-for-one; see statute.Strategy for documentation.
const (
	RoundRobin Strategy = iota
	LeastConnections
	IPHash
	Weighted
)

// HealthCheck is a resolved active health check.
type HealthCheck struct {
	Enabled   bool
	Path      string
	Interval  time.Duration
	Timeout   time.Duration
	Healthy   int
	Unhealthy int
}

// Transport is a resolved transport configuration. The TLS fields form the
// pool's one backend-verification policy, shared by proxy requests and
// health-check probes.
type Transport struct {
	MaxIdleConnsPerHost int
	IdleConnTimeout     time.Duration
	DialTimeout         time.Duration
	TLSHandshakeTimeout time.Duration
	ServerName          string
	RootCAFiles         []string
	InsecureSkipVerify  bool
}

// Route is a resolved route.
type Route struct {
	Pattern    string
	Host       string
	Upstream   *Pool  // nil unless this route proxies
	StaticDir  string // empty unless this route serves static files
	Middleware []Middleware
	Redirect   *Redirect // nil unless this route redirects
}

// Redirect is a resolved route redirect action: the route answers with
// Status and a Location built from Target, whose placeholders are
// substituted from the live request.
type Redirect struct {
	Target string
	Status int
}

// Middleware identifies a single resolved middleware. The Type discriminator
// drives runtime behaviour; only fields relevant to that type are populated.
type Middleware struct {
	Type MiddlewareType

	// Timeout
	Timeout time.Duration

	// RateLimit
	RateLimitPerSecond float64
	RateLimitKey       RateLimitKey

	// Retry
	RetryMax        int
	RetryOnStatuses []int

	// Cache
	CacheTTL time.Duration

	// Compress
	CompressAlgos []CompressAlgo

	// BodyLimit
	BodyLimitBytes int64

	// RequestID
	RequestIDHeader     string
	RequestIDFromHeader string

	// SecurityHeaders — each empty string means "do not emit this header".
	SecHSTS               string
	SecCSP                string
	SecFrameOptions       string
	SecContentTypeOptions bool
	SecReferrerPolicy     string
	SecPermissionsPolicy  string

	// CORS
	CORSOrigins        []string
	CORSAllowAllOrigin bool // explicit "*" in Origins
	CORSMethods        []string
	CORSHeaders        []string
	CORSExposeHeaders  []string
	CORSCredentials    bool
	CORSMaxAge         time.Duration

	// BasicAuth — users is a username -> bcrypt hash map.
	BasicAuthRealm string
	BasicAuthUsers map[string]string

	// IP allow/deny — only one direction is populated per middleware.
	IPCIDRs []string // canonical "1.2.3.0/24"

	// Header mutation — the Type discriminator selects request or response
	// and set, add, or remove. HeaderName is canonical ("X-Robots-Tag");
	// HeaderValue is empty for the remove operations.
	HeaderName  string
	HeaderValue string
}

// MiddlewareType discriminates resolved middleware values.
type MiddlewareType int

// Middleware-type discriminators. Append new entries to this list; never
// insert in the middle — the integer values are part of the JSON resolved
// export contract that downstream tooling depends on.
const (
	MWTimeout MiddlewareType = iota
	MWRateLimit
	MWRetry
	MWCache
	MWCompress
	MWETag
	MWBodyLimit
	MWRequestID
	MWSecurityHeaders
	MWCORS
	MWBasicAuth
	MWAllowIPs
	MWDenyIPs
	MWSetRequestHeader
	MWAddRequestHeader
	MWRemoveRequestHeader
	MWSetResponseHeader
	MWAddResponseHeader
	MWRemoveResponseHeader
)

// RateLimitKey mirrors the surface key.
type RateLimitKey int

// Rate-limit bucket keys. Mirror statute.RateLimitKey.
const (
	KeyClientIP RateLimitKey = iota
	KeyHostHeader
)

// CompressAlgo mirrors the surface algorithm enum.
type CompressAlgo int

// Compression algorithms. Mirror statute.CompressAlgo.
const (
	Gzip CompressAlgo = iota
	Brotli
)

// Defaults is the resolved server defaults.
type Defaults struct {
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	MaxHeaderBytes    int
}

// Observability is the resolved observability configuration.
type Observability struct {
	AccessLog AccessLog
	Metrics   Metrics
	Tracing   Tracing
}

// Tracing is the resolved distributed-tracing configuration.
type Tracing struct {
	Enabled     bool
	Kind        string // "otlp"
	Endpoint    string
	ServiceName string
	Insecure    bool
	SampleRate  float64
}

// AccessLog is the resolved access log configuration.
type AccessLog struct {
	Enabled bool
	Format  string // "json"
	Writer  io.Writer
	Name    string // human label for the writer

	// SampleRate is the fraction of successful requests to record, in (0,1].
	// Errors (status >= 500) and client errors (4xx) are logged regardless;
	// sampling only suppresses successful requests at high volume.
	SampleRate float64
}

// Metrics is the resolved metrics configuration.
type Metrics struct {
	Enabled bool
	Kind    string // "prometheus"
	Addr    string
	Path    string
}

// Shutdown is the resolved shutdown configuration.
type Shutdown struct {
	GracePeriod    time.Duration
	DrainListeners bool
}
