package statute

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"statute.kjanat.dev/internal/parse"
	"statute.kjanat.dev/resolved"
)

// Resolve validates the surface configuration, fills defaults, and produces
// the canonical resolved schema. Resolve is pure: it does not touch the
// network, the filesystem, or process state.
func Resolve(cfg Config) (*resolved.Config, error) {
	out := &resolved.Config{
		Upstreams: make(map[string]*resolved.Pool, len(cfg.Upstreams)),
	}

	defaults, err := resolveDefaults(cfg.Defaults)
	if err != nil {
		return nil, fmt.Errorf("defaults: %w", err)
	}
	out.Defaults = defaults

	if err := resolveUpstreams(cfg.Upstreams, out.Upstreams); err != nil {
		return nil, err
	}
	if err := resolveListeners(cfg.Listeners, out); err != nil {
		return nil, err
	}
	if err := resolveRoutes(cfg.Routes, out); err != nil {
		return nil, err
	}

	docker, err := resolveDocker(cfg.Docker)
	if err != nil {
		return nil, fmt.Errorf("docker: %w", err)
	}
	out.Docker = docker

	obs, err := resolveObservability(cfg.Observability)
	if err != nil {
		return nil, fmt.Errorf("observability: %w", err)
	}
	out.Observability = obs

	sh, err := resolveShutdown(cfg.Shutdown)
	if err != nil {
		return nil, fmt.Errorf("shutdown: %w", err)
	}
	out.Shutdown = sh

	if err := validateResolved(out); err != nil {
		return nil, err
	}
	return out, nil
}

// resolveUpstreams resolves every surface pool into out, keyed by name.
func resolveUpstreams(in map[string]Pool, out map[string]*resolved.Pool) error {
	for name, pool := range in {
		rp, err := resolvePool(name, pool)
		if err != nil {
			return fmt.Errorf("upstream %q: %w", name, err)
		}
		out[name] = rp
	}
	return nil
}

// resolveListeners resolves every surface listener and appends it to out.
func resolveListeners(in []*Listener, out *resolved.Config) error {
	for i, l := range in {
		rl, err := resolveListener(l)
		if err != nil {
			return fmt.Errorf("listener[%d]: %w", i, err)
		}
		out.Listeners = append(out.Listeners, rl)
	}
	return nil
}

// resolveRoutes resolves every surface route and appends it to out.
// Upstream references are looked up in out.Upstreams, so resolveUpstreams
// must run first.
func resolveRoutes(in []*Route, out *resolved.Config) error {
	for i, r := range in {
		rr, err := resolveRoute(r, out.Upstreams)
		if err != nil {
			return fmt.Errorf("route[%d] %q: %w", i, r.pattern, err)
		}
		out.Routes = append(out.Routes, rr)
	}
	return nil
}

func resolveDefaults(d Defaults) (resolved.Defaults, error) {
	rhT, err := parse.DurationOr(d.ReadHeaderTimeout, 5*time.Second)
	if err != nil {
		return resolved.Defaults{}, fmt.Errorf("read_header_timeout: %w", err)
	}
	rT, err := parse.DurationOr(d.ReadTimeout, 0)
	if err != nil {
		return resolved.Defaults{}, fmt.Errorf("read_timeout: %w", err)
	}
	wT, err := parse.DurationOr(d.WriteTimeout, 30*time.Second)
	if err != nil {
		return resolved.Defaults{}, fmt.Errorf("write_timeout: %w", err)
	}
	iT, err := parse.DurationOr(d.IdleTimeout, 120*time.Second)
	if err != nil {
		return resolved.Defaults{}, fmt.Errorf("idle_timeout: %w", err)
	}
	mhb := d.MaxHeaderBytes
	if mhb == 0 {
		mhb = 1 << 20 // 1MB, matches Go's default
	}
	return resolved.Defaults{
		ReadHeaderTimeout: rhT,
		ReadTimeout:       rT,
		WriteTimeout:      wT,
		IdleTimeout:       iT,
		MaxHeaderBytes:    mhb,
	}, nil
}

func resolvePool(name string, p Pool) (*resolved.Pool, error) {
	if len(p.Backends) == 0 {
		return nil, errors.New("pool has no backends")
	}
	rp := &resolved.Pool{
		Name:     name,
		Strategy: resolved.Strategy(p.Strategy),
	}
	for i, b := range p.Backends {
		if strings.TrimSpace(b.Address) == "" {
			return nil, fmt.Errorf("backend[%d]: address is empty", i)
		}
		w := b.Weight
		if w == 0 {
			w = 1
		}
		rp.Backends = append(rp.Backends, resolved.Backend{
			Address: b.Address,
			Weight:  w,
			Backup:  b.Backup,
		})
	}
	hc, err := resolveHealthCheck(p.HealthCheck)
	if err != nil {
		return nil, fmt.Errorf("health_check: %w", err)
	}
	rp.HealthCheck = hc
	tr, err := resolveTransport(p.Transport)
	if err != nil {
		return nil, fmt.Errorf("transport: %w", err)
	}
	rp.Transport = tr
	return rp, nil
}

func resolveHealthCheck(h HealthCheck) (resolved.HealthCheck, error) {
	if h.Path == "" {
		return resolved.HealthCheck{Enabled: false}, nil
	}
	interval, err := parse.DurationOr(h.Interval, 10*time.Second)
	if err != nil {
		return resolved.HealthCheck{}, fmt.Errorf("interval: %w", err)
	}
	timeout, err := parse.DurationOr(h.Timeout, 2*time.Second)
	if err != nil {
		return resolved.HealthCheck{}, fmt.Errorf("timeout: %w", err)
	}
	healthy := h.Healthy
	if healthy == 0 {
		healthy = 2
	}
	unhealthy := h.Unhealthy
	if unhealthy == 0 {
		unhealthy = 3
	}
	return resolved.HealthCheck{
		Enabled:   true,
		Path:      h.Path,
		Interval:  interval,
		Timeout:   timeout,
		Healthy:   healthy,
		Unhealthy: unhealthy,
	}, nil
}

func resolveTransport(t Transport) (resolved.Transport, error) {
	maxIdle := t.MaxIdleConnsPerHost
	if maxIdle == 0 {
		maxIdle = 32
	}
	idle, err := parse.DurationOr(t.IdleConnTimeout, 90*time.Second)
	if err != nil {
		return resolved.Transport{}, fmt.Errorf("idle_conn_timeout: %w", err)
	}
	dial, err := parse.DurationOr(t.DialTimeout, 5*time.Second)
	if err != nil {
		return resolved.Transport{}, fmt.Errorf("dial_timeout: %w", err)
	}
	tlsHs, err := parse.DurationOr(t.TLSHandshakeTimeout, 5*time.Second)
	if err != nil {
		return resolved.Transport{}, fmt.Errorf("tls_handshake_timeout: %w", err)
	}
	return resolved.Transport{
		MaxIdleConnsPerHost: maxIdle,
		IdleConnTimeout:     idle,
		DialTimeout:         dial,
		TLSHandshakeTimeout: tlsHs,
	}, nil
}

// resolveDocker fills provider defaults and parses the refresh interval.
// The endpoint's scheme is validated here so a typo fails at Resolve time,
// not when the runtime first dials the daemon.
func resolveDocker(d *DockerConfig) (*resolved.Docker, error) {
	if d == nil {
		return nil, nil
	}
	endpoint := d.endpoint
	if endpoint == "" {
		endpoint = "unix:///var/run/docker.sock"
	}
	if !strings.HasPrefix(endpoint, "unix://") && !strings.HasPrefix(endpoint, "tcp://") && !strings.HasPrefix(endpoint, "http://") {
		return nil, fmt.Errorf("endpoint %q: must be unix:// or tcp://", endpoint)
	}
	refresh, err := parse.DurationOr(d.refresh, 0)
	if err != nil {
		return nil, fmt.Errorf("refresh: %w", err)
	}
	return &resolved.Docker{
		Endpoint:         endpoint,
		Network:          d.network,
		ExposedByDefault: d.exposedByDefault,
		TraefikLabels:    d.traefikLabels,
		Refresh:          refresh,
	}, nil
}

func resolveListener(l *Listener) (*resolved.Listener, error) {
	if l == nil {
		return nil, errors.New("nil listener")
	}
	if strings.TrimSpace(l.addr) == "" {
		return nil, errors.New("address is empty")
	}
	rl := &resolved.Listener{
		Addr:             l.addr,
		Scheme:           l.scheme,
		Redirect:         l.redirect,
		EnableHTTP2:      l.enableHTTP2,
		HTTP3Addr:        l.http3Addr,
		BehindCloudflare: l.behindCF,
	}
	if err := validateListenerTLSPresence(l); err != nil {
		return nil, err
	}
	if l.autoTLS != nil {
		at, err := resolveAutoTLS(l.autoTLS)
		if err != nil {
			return nil, err
		}
		rl.AutoTLS = at
	}
	if l.staticTLS != nil {
		st, err := resolveStaticTLS(l.staticTLS)
		if err != nil {
			return nil, err
		}
		rl.StaticTLS = st
	}
	return rl, nil
}

// validateListenerTLSPresence rejects an HTTPS content listener that carries
// no TLS material. Redirect-only listeners and plain HTTP need none.
func validateListenerTLSPresence(l *Listener) error {
	if l.scheme == schemeHTTPS && l.redirect == "" && l.autoTLS == nil && l.staticTLS == nil {
		return errors.New("https listener requires AutoTLS or StaticTLS")
	}
	return nil
}

// resolveAutoTLS validates an AutoTLS surface config (domains, email,
// storage, optional Cloudflare DNS-01) and produces its resolved form.
func resolveAutoTLS(a *AutoTLSConfig) (*resolved.AutoTLS, error) {
	if len(a.Domains) == 0 {
		return nil, errors.New("auto_tls: at least one domain required")
	}
	if a.email == "" {
		return nil, errors.New("auto_tls: email required for ACME registration")
	}
	if a.storage == "" {
		return nil, errors.New("auto_tls: storage path required for cert persistence")
	}
	at := &resolved.AutoTLS{
		Domains: append([]string(nil), a.Domains...),
		Email:   a.email,
		Storage: a.storage,
	}
	if a.dns01 != nil {
		if a.dns01.apiToken == "" {
			return nil, errors.New("auto_tls.cloudflare_dns01: api token is required")
		}
		at.DNS01 = &resolved.CloudflareDNS01{
			APIToken: a.dns01.apiToken,
			ZoneID:   a.dns01.zoneID,
		}
	}
	return at, nil
}

// resolveStaticTLS validates that both cert and key paths are set and
// produces the resolved StaticTLS form.
func resolveStaticTLS(s *StaticTLSConfig) (*resolved.StaticTLS, error) {
	if s.CertFile == "" || s.KeyFile == "" {
		return nil, errors.New("static_tls: cert_file and key_file required")
	}
	return &resolved.StaticTLS{
		CertFile: s.CertFile,
		KeyFile:  s.KeyFile,
	}, nil
}

func resolveRoute(r *Route, pools map[string]*resolved.Pool) (*resolved.Route, error) {
	if r == nil {
		return nil, errors.New("nil route")
	}
	if r.pattern == "" {
		return nil, errors.New("pattern is empty")
	}
	rr := &resolved.Route{
		Pattern:   r.pattern,
		Host:      r.host,
		StaticDir: r.staticDir,
	}
	if err := resolveRouteTarget(r, pools, rr); err != nil {
		return nil, err
	}
	mws, err := resolveMiddlewares(r.middleware)
	if err != nil {
		return nil, err
	}
	rr.Middleware = mws
	return rr, nil
}

// resolveRouteTarget validates the ProxyTo/Serve choice and, for a proxy
// route, dereferences the named upstream into rr.Upstream.
func resolveRouteTarget(r *Route, pools map[string]*resolved.Pool, rr *resolved.Route) error {
	switch {
	case r.upstream != "" && r.staticDir != "":
		return errors.New("route has both ProxyTo and Serve; pick one")
	case r.upstream == "" && r.staticDir == "":
		return errors.New("route has neither ProxyTo nor Serve")
	case r.upstream != "":
		pool, ok := pools[r.upstream]
		if !ok {
			return fmt.Errorf("unknown upstream %q", r.upstream)
		}
		rr.Upstream = pool
	}
	return nil
}

// resolveMiddlewares resolves a route's middleware list in order,
// returning nil for an empty list so an unmiddlewared route stays nil.
func resolveMiddlewares(mws []Middleware) ([]resolved.Middleware, error) {
	if len(mws) == 0 {
		return nil, nil
	}
	out := make([]resolved.Middleware, 0, len(mws))
	for i, mw := range mws {
		rmw, err := resolveMiddleware(mw)
		if err != nil {
			return nil, fmt.Errorf("middleware[%d]: %w", i, err)
		}
		out = append(out, rmw)
	}
	return out, nil
}

// Every surface middleware implements one of these resolver interfaces. Types
// that cannot fail avoid manufacturing an error result solely for dispatch.
type fallibleMiddleware interface {
	resolve() (resolved.Middleware, error)
}

type infallibleMiddleware interface {
	resolve() resolved.Middleware
}

func resolveMiddleware(mw Middleware) (resolved.Middleware, error) {
	switch rm := mw.(type) {
	case fallibleMiddleware:
		return rm.resolve()
	case infallibleMiddleware:
		return rm.resolve(), nil
	default:
		return resolved.Middleware{}, fmt.Errorf("unknown middleware type %T", mw)
	}
}

func (m *timeoutMW) resolve() (resolved.Middleware, error)   { return resolveTimeoutMW(m) }
func (m *rateLimitMW) resolve() (resolved.Middleware, error) { return resolveRateLimitMW(m) }
func (m *retryMW) resolve() (resolved.Middleware, error)     { return resolveRetryMW(m) }
func (m *cacheMW) resolve() (resolved.Middleware, error)     { return resolveCacheMW(m) }

func (m *compressMW) resolve() resolved.Middleware { return resolveCompressMW(m) }

func (*etagMW) resolve() resolved.Middleware {
	return resolved.Middleware{Type: resolved.MWETag}
}
func (m *bodyLimitMW) resolve() (resolved.Middleware, error) { return resolveBodyLimitMW(m) }

func (m *requestIDMW) resolve() resolved.Middleware {
	return resolved.Middleware{
		Type:                resolved.MWRequestID,
		RequestIDHeader:     m.header,
		RequestIDFromHeader: m.fromHeader,
	}
}

func (m *securityHeadersMW) resolve() (resolved.Middleware, error) {
	return resolveSecurityHeadersMW(m)
}
func (m *allowIPsMW) resolve() (resolved.Middleware, error)  { return resolveAllowIPsMW(m) }
func (m *denyIPsMW) resolve() (resolved.Middleware, error)   { return resolveDenyIPsMW(m) }
func (m *basicAuthMW) resolve() (resolved.Middleware, error) { return resolveBasicAuthMW(m) }
func (m *corsMW) resolve() (resolved.Middleware, error)      { return resolveCORSMW(m) }
func (m *headerMW) resolve() (resolved.Middleware, error)    { return resolveHeaderMW(m) }

// resolveTimeoutMW parses the timeout duration string.
func resolveTimeoutMW(m *timeoutMW) (resolved.Middleware, error) {
	d, err := parse.Duration(m.dur)
	if err != nil {
		return resolved.Middleware{}, fmt.Errorf("timeout: %w", err)
	}
	return resolved.Middleware{Type: resolved.MWTimeout, Timeout: d}, nil
}

// resolveRateLimitMW parses the rate string into requests/second and
// carries the rate-limit key.
func resolveRateLimitMW(m *rateLimitMW) (resolved.Middleware, error) {
	rate, err := parse.Rate(m.rate)
	if err != nil {
		return resolved.Middleware{}, fmt.Errorf("rate_limit: %w", err)
	}
	return resolved.Middleware{
		Type:               resolved.MWRateLimit,
		RateLimitPerSecond: rate,
		RateLimitKey:       resolved.RateLimitKey(m.key),
	}, nil
}

// resolveRetryMW validates that max attempts is >= 1 and copies the
// retry-on-status list.
func resolveRetryMW(m *retryMW) (resolved.Middleware, error) {
	if m.max < 1 {
		return resolved.Middleware{}, errors.New("retry: max must be >= 1")
	}
	return resolved.Middleware{
		Type:            resolved.MWRetry,
		RetryMax:        m.max,
		RetryOnStatuses: append([]int(nil), m.onStatuses...),
	}, nil
}

// resolveCacheMW parses the cache TTL duration string.
func resolveCacheMW(m *cacheMW) (resolved.Middleware, error) {
	d, err := parse.Duration(m.ttl)
	if err != nil {
		return resolved.Middleware{}, fmt.Errorf("cache: %w", err)
	}
	return resolved.Middleware{Type: resolved.MWCache, CacheTTL: d}, nil
}

// resolveCompressMW maps the requested compression algorithms to their
// resolved form. It cannot fail.
func resolveCompressMW(m *compressMW) resolved.Middleware {
	algos := make([]resolved.CompressAlgo, 0, len(m.algos))
	for _, a := range m.algos {
		algos = append(algos, resolved.CompressAlgo(a))
	}
	return resolved.Middleware{Type: resolved.MWCompress, CompressAlgos: algos}
}

// resolveBodyLimitMW parses the size string and requires a positive limit.
func resolveBodyLimitMW(m *bodyLimitMW) (resolved.Middleware, error) {
	n, err := parse.Size(m.size)
	if err != nil {
		return resolved.Middleware{}, fmt.Errorf("body_limit: %w", err)
	}
	if n <= 0 {
		return resolved.Middleware{}, errors.New("body_limit: size must be positive")
	}
	return resolved.Middleware{Type: resolved.MWBodyLimit, BodyLimitBytes: n}, nil
}

// resolveSecurityHeadersMW formats the optional HSTS duration and copies
// the remaining header policy values.
func resolveSecurityHeadersMW(m *securityHeadersMW) (resolved.Middleware, error) {
	hstsHeader := ""
	if m.hsts != "" {
		d, err := parse.Duration(m.hsts)
		if err != nil {
			return resolved.Middleware{}, fmt.Errorf("security_headers.hsts: %w", err)
		}
		hstsHeader = formatHSTS(d)
	}
	return resolved.Middleware{
		Type:                  resolved.MWSecurityHeaders,
		SecHSTS:               hstsHeader,
		SecCSP:                m.csp,
		SecFrameOptions:       m.frameOptions,
		SecContentTypeOptions: m.contentTypeOptions,
		SecReferrerPolicy:     m.referrerPolicy,
		SecPermissionsPolicy:  m.permissionsPolicy,
	}, nil
}

// unsettableRequestHeaders are the request fields Go carries outside the
// header map, where a mutation would be a silent no-op: net/http writes them
// from Request.Host, Request.ContentLength, and Request.TransferEncoding and
// excludes the header-map entries when it writes the request. Rejecting them
// at resolve time turns a configuration that cannot work into a startup error.
var unsettableRequestHeaders = map[string]string{
	"Host":              "Go keeps the request authority in Request.Host",
	"Content-Length":    "Go frames the body from Request.ContentLength",
	"Transfer-Encoding": "Go frames the body from Request.TransferEncoding",
}

// resolveHeaderMW validates one header mutation and canonicalises its name.
func resolveHeaderMW(m *headerMW) (resolved.Middleware, error) {
	label := headerMWLabel(m.op)
	name, err := parse.HeaderName(m.name)
	if err != nil {
		return resolved.Middleware{}, fmt.Errorf("%s: %w", label, err)
	}
	value, err := parse.HeaderValue(m.value)
	if err != nil {
		return resolved.Middleware{}, fmt.Errorf("%s: %w", label, err)
	}
	if reason, ok := unsettableRequestHeaders[name]; ok && isRequestHeaderOp(m.op) {
		return resolved.Middleware{}, fmt.Errorf("%s: %q cannot be rewritten on a request; %s", label, name, reason)
	}
	return resolved.Middleware{Type: m.op, HeaderName: name, HeaderValue: value}, nil
}

// resolveAllowIPsMW canonicalises the allow-list CIDRs.
func resolveAllowIPsMW(m *allowIPsMW) (resolved.Middleware, error) {
	canon, err := resolveCIDRs(m.cidrs)
	if err != nil {
		return resolved.Middleware{}, fmt.Errorf("allow_ips: %w", err)
	}
	return resolved.Middleware{Type: resolved.MWAllowIPs, IPCIDRs: canon}, nil
}

// resolveDenyIPsMW canonicalises the deny-list CIDRs.
func resolveDenyIPsMW(m *denyIPsMW) (resolved.Middleware, error) {
	canon, err := resolveCIDRs(m.cidrs)
	if err != nil {
		return resolved.Middleware{}, fmt.Errorf("deny_ips: %w", err)
	}
	return resolved.Middleware{Type: resolved.MWDenyIPs, IPCIDRs: canon}, nil
}

// resolveBasicAuthMW requires a non-empty user map and verifies every
// value is a bcrypt hash.
func resolveBasicAuthMW(m *basicAuthMW) (resolved.Middleware, error) {
	if len(m.users) == 0 {
		return resolved.Middleware{}, errors.New("basic_auth: users map is empty")
	}
	users := make(map[string]string, len(m.users))
	for name, hash := range m.users {
		if !isBCryptHash(hash) {
			return resolved.Middleware{}, fmt.Errorf("basic_auth: user %q: value is not a bcrypt hash (must be $2a$/$2b$/$2y$ prefixed)", name)
		}
		users[name] = hash
	}
	return resolved.Middleware{
		Type:           resolved.MWBasicAuth,
		BasicAuthRealm: m.realm,
		BasicAuthUsers: users,
	}, nil
}

// resolveCORSMW splits explicit origins from the wildcard, rejects a
// credentialed wildcard, and parses the optional max-age.
func resolveCORSMW(m *corsMW) (resolved.Middleware, error) {
	if len(m.origins) == 0 {
		return resolved.Middleware{}, errors.New("cors: at least one Origin must be configured")
	}
	allowAll := false
	var explicit []string
	for _, o := range m.origins {
		if o == "*" {
			allowAll = true
			continue
		}
		explicit = append(explicit, o)
	}
	if allowAll && m.credentials {
		return resolved.Middleware{}, errors.New("cors: wildcard origin '*' is incompatible with Credentials() per the CORS spec")
	}
	maxAge := time.Duration(0)
	if m.maxAge != "" {
		d, err := parse.Duration(m.maxAge)
		if err != nil {
			return resolved.Middleware{}, fmt.Errorf("cors.max_age: %w", err)
		}
		maxAge = d
	}
	return resolved.Middleware{
		Type:               resolved.MWCORS,
		CORSAllowAllOrigin: allowAll,
		CORSOrigins:        explicit,
		CORSMethods:        append([]string(nil), m.methods...),
		CORSHeaders:        append([]string(nil), m.headers...),
		CORSExposeHeaders:  append([]string(nil), m.exposeHeaders...),
		CORSCredentials:    m.credentials,
		CORSMaxAge:         maxAge,
	}, nil
}

func resolveObservability(o Observability) (resolved.Observability, error) {
	out := resolved.Observability{}
	if o.AccessLog != nil {
		switch al := o.AccessLog.(type) {
		case *jsonLog:
			out.AccessLog = resolved.AccessLog{
				Enabled:    true,
				Format:     accessLogFormatJSON,
				Writer:     al.dest.Writer(),
				Name:       al.dest.Name(),
				SampleRate: al.sampleRate,
			}
		default:
			return resolved.Observability{}, fmt.Errorf("unknown access log type %T", o.AccessLog)
		}
	}
	if o.Metrics != nil {
		switch m := o.Metrics.(type) {
		case prometheusMetrics:
			if m.addr == "" {
				return resolved.Observability{}, errors.New("metrics: addr required")
			}
			path := m.path
			if path == "" {
				path = "/metrics"
			}
			out.Metrics = resolved.Metrics{
				Enabled: true,
				Kind:    "prometheus",
				Addr:    m.addr,
				Path:    path,
			}
		default:
			return resolved.Observability{}, fmt.Errorf("unknown metrics type %T", o.Metrics)
		}
	}
	if o.Tracing != nil {
		switch t := o.Tracing.(type) {
		case *otlpTracing:
			if t.endpoint == "" {
				return resolved.Observability{}, errors.New("tracing: endpoint required")
			}
			out.Tracing = resolved.Tracing{
				Enabled:     true,
				Kind:        "otlp",
				Endpoint:    t.endpoint,
				ServiceName: t.serviceName,
				Insecure:    t.insecure,
				SampleRate:  t.sampleRate,
			}
		default:
			return resolved.Observability{}, fmt.Errorf("unknown tracing type %T", o.Tracing)
		}
	}
	return out, nil
}

func resolveShutdown(s Shutdown) (resolved.Shutdown, error) {
	d, err := parse.DurationOr(s.GracePeriod, 30*time.Second)
	if err != nil {
		return resolved.Shutdown{}, fmt.Errorf("grace_period: %w", err)
	}
	return resolved.Shutdown{
		GracePeriod:    d,
		DrainListeners: s.DrainListeners,
	}, nil
}

func validateResolved(c *resolved.Config) error {
	if len(c.Listeners) == 0 {
		return errors.New("at least one listener required")
	}
	// Routes are optional in principle (a redirect-only deployment is valid),
	// but a redirect-only listener with no routes and no other listener is
	// almost certainly a misconfiguration.
	hasContentListener := false
	for _, l := range c.Listeners {
		if l.Redirect == "" {
			hasContentListener = true
			break
		}
	}
	if !hasContentListener && len(c.Routes) > 0 {
		return errors.New("routes declared but no listener serves content (all listeners are redirects)")
	}
	return nil
}

// resolveCIDRs validates and canonicalises a list of CIDR strings. Every
// entry must be parseable as a netip.Prefix; the resolved list is the
// canonical Masked form so that an entry like "10.0.0.1/24" is stored as
// "10.0.0.0/24".
func resolveCIDRs(cidrs []string) ([]string, error) {
	if len(cidrs) == 0 {
		return nil, errors.New("at least one CIDR required")
	}
	out := make([]string, 0, len(cidrs))
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %w", c, err)
		}
		out = append(out, p.Masked().String())
	}
	return out, nil
}
