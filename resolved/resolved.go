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
	"net/http"
	"time"
)

// Config is the resolved top-level configuration.
type Config struct {
	Listeners []*Listener
	Upstreams map[string]*Pool
	Routes    []*Route
	Docker    *Docker // nil unless Docker label discovery is enabled

	// Fallback is the handler after static routes, the current Docker
	// generation, and that generation's refusal envelopes; nil is 404.
	Fallback http.Handler `json:"-"`
	// HasFallback is true when Fallback is non-nil. The JSON export uses
	// this marker; the handler itself does not serialize.
	HasFallback bool

	Defaults      Defaults
	Observability Observability
	Shutdown      Shutdown
}

// Docker is the resolved Docker label-discovery provider configuration.
// The provider's output (label-derived routes and pools, and the refusal
// tombstones standing in for the registrations it had to drop) is runtime
// state. Discovery settings and immutable code-owned policy registries belong
// in the resolved schema; discovered backends and routes do not.
// Tombstones belong to one generation, are replaced with it, and describe
// containers.
type Docker struct {
	Endpoint         string
	Network          string
	ExposedByDefault bool
	TraefikLabels    bool
	// Refresh is the periodic full-resync interval; zero means events only.
	Refresh time.Duration
	// Middleware is the code-owned registry of named middleware chains that
	// container label references (traefik.http.routers.<r>.middlewares)
	// resolve against. Labels select these compiled policies by exact name;
	// they never define middleware of their own.
	Middleware map[string][]Middleware
	// DefaultMiddleware is applied to every Docker-discovered route,
	// outermost — before label-referenced chains and label hints.
	DefaultMiddleware []Middleware
	// PoolPolicy is immutable, code-owned pool policy keyed by exact
	// discovered-service identity. Docker still supplies the service's
	// backends, strategy, and routes at runtime.
	PoolPolicy map[string]PoolPolicy
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

	// AutoTLSSources and StaticTLSSources hold every TLS source declared on
	// the listener, in declaration order. A listener may mix ACME challenge
	// policies and static material on one port; the runtime picks a source
	// per handshake by SNI hostname — exact name first, then wildcard
	// pattern, then the hostless static fallback. AutoTLS and StaticTLS
	// above mirror the first source of each kind so single-source tooling
	// keeps working; multi-source aware tooling should read the slices.
	AutoTLSSources   []*AutoTLS
	StaticTLSSources []*StaticTLS

	// TLSPolicy is the listener's downstream TLS protocol policy, shared by
	// its TCP and QUIC listeners. Nil means none was declared and the
	// runtime's defaults apply: minimum TLS 1.2, no upper bound, and Go's
	// own TLS 1.2 cipher-suite selection.
	TLSPolicy *TLSPolicy

	// BehindCloudflare indicates the listener is fronted by Cloudflare. The
	// runtime suppresses TLS-ALPN-01 challenges (HTTP-01 only) and trusts
	// CF-Connecting-IP / True-Client-IP for client IP attribution.
	BehindCloudflare bool

	// TrustedProxies are CIDR ranges whose members may assert the client IP
	// through ClientIPHeader; peers outside them are their own clients and
	// their forwarded headers are ignored. Empty means no per-peer trust is
	// configured on this listener.
	TrustedProxies []string
	ClientIPHeader string
}

// TLSPolicy is a resolved downstream TLS protocol policy: the version
// window a listener negotiates and the TLS 1.2 cipher suites it permits.
type TLSPolicy struct {
	// MinVersion is the lowest protocol version the listener negotiates,
	// as "1.2" or "1.3". Empty means unset: the runtime's floor applies.
	MinVersion string
	// MaxVersion is the highest protocol version the listener negotiates,
	// as "1.2" or "1.3". Empty means unset: no upper bound is imposed.
	MaxVersion string
	// CipherSuites are the permitted TLS 1.2 cipher suites, by IANA name
	// (e.g. "TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256"), in the order they
	// were declared. Empty means unset: the TLS stack's own selection
	// applies. TLS 1.3 suites are fixed by the protocol and never listed.
	CipherSuites []string
}

// AutoTLS is the resolved ACME configuration.
type AutoTLS struct {
	Domains []string
	Email   string
	Storage string
	DNS01   *CloudflareDNS01

	// Directory is the ACME directory URL. Always non-empty in the
	// resolved model: Resolve fills in Let's Encrypt production when the
	// surface config leaves it unset, so the exported configuration shows
	// the directory actually used and the runtime has no second
	// defaulting site.
	Directory string

	// Challenge is the source's ACME challenge policy. ChallengeDNS01 holds
	// exactly when DNS01 is non-nil.
	Challenge Challenge
}

// Challenge identifies the ACME challenge policy of one AutoTLS source.
type Challenge int

const (
	// ChallengeAuto lets the runtime pick: TLS-ALPN-01 where the listener
	// can advertise it, with fallback to HTTP-01 (default).
	ChallengeAuto Challenge = iota
	// ChallengeHTTP01 pins issuance to HTTP-01 through the in-tree ACME
	// manager; TLS-ALPN-01 is never attempted or advertised for the source.
	ChallengeHTTP01
	// ChallengeDNS01 issues over DNS-01 via the configured DNS provider.
	ChallengeDNS01
)

// CloudflareDNS01 is the resolved DNS-01 configuration. When non-nil, the
// runtime uses Cloudflare's DNS API to satisfy challenges instead of HTTP-01.
type CloudflareDNS01 struct {
	APIToken string
	ZoneID   string // optional; empty means auto-discover

	// Propagation is the source's DNS propagation policy: how long the
	// runtime waits, and which resolvers it verifies against, after
	// publishing the challenge TXT record and before asking the CA to
	// validate it. Nil means none was declared and the runtime's fixed
	// default wait applies.
	Propagation *DNSPropagation
}

// DNSPropagation is a resolved DNS-01 propagation policy. It has two
// independent halves, either or both of which may be active: a fixed Delay
// that always elapses first, and — when Resolvers is non-empty — a polling
// loop that queries every listed resolver for the challenge TXT record
// until they all serve the expected value.
type DNSPropagation struct {
	// Delay is the fixed wait after publishing the record, before any
	// polling or validation. Zero means none: with Resolvers set, polling
	// begins immediately.
	Delay time.Duration
	// Timeout is the deadline for the polling loop, measured from the end
	// of Delay. Zero exactly when Resolvers is empty, since there is then
	// nothing to poll.
	Timeout time.Duration
	// Interval is the cadence between polling rounds; the first round runs
	// immediately, not after one interval. Zero exactly when Resolvers is
	// empty.
	Interval time.Duration
	// Resolvers are the "host:port" DNS servers that must all serve the
	// expected TXT value before validation is requested, in declaration
	// order. Empty means no polling: only Delay applies.
	Resolvers []string
}

// StaticTLS is the resolved static-cert configuration.
type StaticTLS struct {
	CertFile string
	KeyFile  string

	// Host scopes the certificate to one SNI name — an exact hostname or a
	// wildcard pattern like "*.bar.example" covering exactly one extra
	// label. Empty means the source is the listener's fallback: it serves
	// names no other source covers, and clients that send no SNI at all.
	Host string
}

// Pool is a resolved upstream pool.
type Pool struct {
	Name               string
	Backends           []Backend
	Strategy           Strategy
	HealthCheck        HealthCheck
	PassiveHealthCheck PassiveHealthCheck
	Transport          Transport
	// UpstreamHost is the pool's outgoing Host header policy; HostValue
	// carries the fixed name when the policy is HostExplicit.
	UpstreamHost HostPolicy
	HostValue    string
}

// PoolPolicy is normalized code-owned policy for a Docker-discovered pool.
// Its service backends, strategy, and routes remain dynamic runtime state.
type PoolPolicy struct {
	HealthCheck        HealthCheck
	PassiveHealthCheck PassiveHealthCheck
	Transport          Transport
	UpstreamHost       HostPolicy
	HostValue          string
}

// HostPolicy selects the Host header backends receive.
type HostPolicy int

const (
	// HostClient forwards the client's original Host header (default).
	HostClient HostPolicy = iota
	// HostTarget sends each backend its own host, from its address.
	HostTarget
	// HostExplicit sends the fixed name in Pool.HostValue.
	HostExplicit
)

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
	// Host is the probe Host override; empty derives the probe host from
	// the pool's UpstreamHost policy.
	Host string
	// Statuses are the accepted probe statuses; empty means 200-399.
	Statuses []int
}

// PassiveHealthCheck is a resolved passive health policy: a backend is
// excluded from selection while MaxFailures failed proxy attempts fall
// inside the sliding FailureWindow.
type PassiveHealthCheck struct {
	Enabled       bool
	FailureWindow time.Duration
	MaxFailures   int
}

// Transport is a resolved transport configuration. The TLS fields form the
// pool's one backend-verification policy, shared by proxy requests and
// health-check probes.
type Transport struct {
	MaxIdleConnsPerHost int
	IdleConnTimeout     time.Duration
	DialTimeout         time.Duration
	TLSHandshakeTimeout time.Duration
	// ResponseHeaderTimeout bounds the wait for upstream response headers;
	// zero keeps Go's default of no timeout.
	ResponseHeaderTimeout time.Duration
	// FlushInterval is the reverse-proxy response flush interval; zero
	// means no periodic flushing (detected streaming responses still
	// flush immediately).
	FlushInterval      time.Duration
	ServerName         string
	RootCAFiles        []string
	InsecureSkipVerify bool
}

// Route is a resolved route. Exactly one of the four actions is set:
// Upstream (proxy), StaticDir (static files), Redirect, or Handler
// (in-process handler, flagged by HandlerRoute).
type Route struct {
	Pattern   string
	Host      string
	Upstream  *Pool  // nil unless this route proxies
	StaticDir string // empty unless this route serves static files
	// Handler is the in-process handler a handler route serves. It is an
	// opaque immutable reference carried through from the surface config,
	// not mutable runtime state, and it cannot serialize — the
	// HandlerRoute marker stands in for it in the JSON export.
	Handler http.Handler `json:"-"`
	// HandlerRoute is true when this route serves an in-process handler.
	HandlerRoute bool
	Middleware   []Middleware
	Redirect     *Redirect // nil unless this route redirects
	// ClientIPCIDRs are canonical CIDR ranges the client must fall inside
	// for the route to match; empty means any client. A non-matching client
	// falls through to the next route.
	ClientIPCIDRs []string
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

	// Path rewrite — the Type discriminator selects the operation.
	PathPrefix      string // strip/add: normalized prefix, leading "/", no trailing "/"
	PathPattern     string // rewrite: RE2 source, compile-validated at resolve
	PathReplacement string // replace: target path (escaped form as given); rewrite: replacement with $1 references
	PathQuery       string // replace: explicit query without "?", only meaningful when PathQuerySet
	PathQuerySet    bool   // replace: true when the target carried a "?" (distinguishes clearing from preserving)
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
	MWStripPrefix
	MWAddPrefix
	MWReplacePath
	MWRewritePath
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
	Health    Health
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

	// Statuses restricts logging to requests whose final status falls in
	// one of the ranges. The filter is a hard gate ahead of every other
	// logging rule, including "errors are always logged": a status outside
	// every range is never logged, and within the allowed ranges errors
	// still bypass sampling. Empty means no filtering. Ranges are
	// normalized: sorted ascending and overlapping or adjacent ranges
	// merged.
	Statuses []StatusRange
}

// StatusRange is one inclusive HTTP status range of an access-log status
// filter.
type StatusRange struct {
	From int
	To   int
}

// Metrics is the resolved metrics configuration.
type Metrics struct {
	Enabled bool
	Kind    string // "prometheus"
	Addr    string
	Path    string
}

// Health is the resolved process health endpoint configuration. Liveness
// serves at Path; readiness serves at Path+"/ready".
type Health struct {
	Enabled bool
	Addr    string
	Path    string
}

// Shutdown is the resolved shutdown configuration.
type Shutdown struct {
	GracePeriod    time.Duration
	DrainListeners bool
}
