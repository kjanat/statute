package statute

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/kjanat/statute/resolved"
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

	for name, pool := range cfg.Upstreams {
		rp, err := resolvePool(name, pool)
		if err != nil {
			return nil, fmt.Errorf("upstream %q: %w", name, err)
		}
		out.Upstreams[name] = rp
	}

	for i, l := range cfg.Listeners {
		rl, err := resolveListener(l)
		if err != nil {
			return nil, fmt.Errorf("listener[%d]: %w", i, err)
		}
		out.Listeners = append(out.Listeners, rl)
	}

	for i, r := range cfg.Routes {
		rr, err := resolveRoute(r, out.Upstreams)
		if err != nil {
			return nil, fmt.Errorf("route[%d] %q: %w", i, r.pattern, err)
		}
		out.Routes = append(out.Routes, rr)
	}

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

func resolveDefaults(d Defaults) (resolved.Defaults, error) {
	rhT, err := parseDurationOr(d.ReadHeaderTimeout, 5*time.Second)
	if err != nil {
		return resolved.Defaults{}, fmt.Errorf("read_header_timeout: %w", err)
	}
	rT, err := parseDurationOr(d.ReadTimeout, 0)
	if err != nil {
		return resolved.Defaults{}, fmt.Errorf("read_timeout: %w", err)
	}
	wT, err := parseDurationOr(d.WriteTimeout, 30*time.Second)
	if err != nil {
		return resolved.Defaults{}, fmt.Errorf("write_timeout: %w", err)
	}
	iT, err := parseDurationOr(d.IdleTimeout, 120*time.Second)
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
	interval, err := parseDurationOr(h.Interval, 10*time.Second)
	if err != nil {
		return resolved.HealthCheck{}, fmt.Errorf("interval: %w", err)
	}
	timeout, err := parseDurationOr(h.Timeout, 2*time.Second)
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
	idle, err := parseDurationOr(t.IdleConnTimeout, 90*time.Second)
	if err != nil {
		return resolved.Transport{}, fmt.Errorf("idle_conn_timeout: %w", err)
	}
	dial, err := parseDurationOr(t.DialTimeout, 5*time.Second)
	if err != nil {
		return resolved.Transport{}, fmt.Errorf("dial_timeout: %w", err)
	}
	tlsHs, err := parseDurationOr(t.TLSHandshakeTimeout, 5*time.Second)
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
	if l.scheme == schemeHTTPS && l.redirect == "" {
		if l.autoTLS == nil && l.staticTLS == nil {
			return nil, errors.New("https listener requires AutoTLS or StaticTLS")
		}
	}
	if l.autoTLS != nil {
		if len(l.autoTLS.Domains) == 0 {
			return nil, errors.New("auto_tls: at least one domain required")
		}
		if l.autoTLS.email == "" {
			return nil, errors.New("auto_tls: email required for ACME registration")
		}
		if l.autoTLS.storage == "" {
			return nil, errors.New("auto_tls: storage path required for cert persistence")
		}
		rl.AutoTLS = &resolved.AutoTLS{
			Domains: append([]string(nil), l.autoTLS.Domains...),
			Email:   l.autoTLS.email,
			Storage: l.autoTLS.storage,
		}
		if l.autoTLS.dns01 != nil {
			if l.autoTLS.dns01.apiToken == "" {
				return nil, errors.New("auto_tls.cloudflare_dns01: api token is required")
			}
			rl.AutoTLS.DNS01 = &resolved.CloudflareDNS01{
				APIToken: l.autoTLS.dns01.apiToken,
				ZoneID:   l.autoTLS.dns01.zoneID,
			}
		}
	}
	if l.staticTLS != nil {
		if l.staticTLS.CertFile == "" || l.staticTLS.KeyFile == "" {
			return nil, errors.New("static_tls: cert_file and key_file required")
		}
		rl.StaticTLS = &resolved.StaticTLS{
			CertFile: l.staticTLS.CertFile,
			KeyFile:  l.staticTLS.KeyFile,
		}
	}
	return rl, nil
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
	switch {
	case r.upstream != "" && r.staticDir != "":
		return nil, errors.New("route has both ProxyTo and Serve; pick one")
	case r.upstream == "" && r.staticDir == "":
		return nil, errors.New("route has neither ProxyTo nor Serve")
	case r.upstream != "":
		pool, ok := pools[r.upstream]
		if !ok {
			return nil, fmt.Errorf("unknown upstream %q", r.upstream)
		}
		rr.Upstream = pool
	}
	for i, mw := range r.middleware {
		rmw, err := resolveMiddleware(mw)
		if err != nil {
			return nil, fmt.Errorf("middleware[%d]: %w", i, err)
		}
		rr.Middleware = append(rr.Middleware, rmw)
	}
	return rr, nil
}

func resolveMiddleware(mw Middleware) (resolved.Middleware, error) {
	switch m := mw.(type) {
	case *timeoutMW:
		d, err := parseDuration(m.dur)
		if err != nil {
			return resolved.Middleware{}, fmt.Errorf("timeout: %w", err)
		}
		return resolved.Middleware{Type: resolved.MWTimeout, Timeout: d}, nil

	case *rateLimitMW:
		rate, err := parseRate(m.rate)
		if err != nil {
			return resolved.Middleware{}, fmt.Errorf("rate_limit: %w", err)
		}
		return resolved.Middleware{
			Type:               resolved.MWRateLimit,
			RateLimitPerSecond: rate,
			RateLimitKey:       resolved.RateLimitKey(m.key),
		}, nil

	case *retryMW:
		if m.max < 1 {
			return resolved.Middleware{}, errors.New("retry: max must be >= 1")
		}
		return resolved.Middleware{
			Type:            resolved.MWRetry,
			RetryMax:        m.max,
			RetryOnStatuses: append([]int(nil), m.onStatuses...),
		}, nil

	case *cacheMW:
		d, err := parseDuration(m.ttl)
		if err != nil {
			return resolved.Middleware{}, fmt.Errorf("cache: %w", err)
		}
		return resolved.Middleware{Type: resolved.MWCache, CacheTTL: d}, nil

	case *compressMW:
		algos := make([]resolved.CompressAlgo, 0, len(m.algos))
		for _, a := range m.algos {
			algos = append(algos, resolved.CompressAlgo(a))
		}
		return resolved.Middleware{Type: resolved.MWCompress, CompressAlgos: algos}, nil

	case *etagMW:
		return resolved.Middleware{Type: resolved.MWETag}, nil

	case *bodyLimitMW:
		n, err := parseSize(m.size)
		if err != nil {
			return resolved.Middleware{}, fmt.Errorf("body_limit: %w", err)
		}
		if n <= 0 {
			return resolved.Middleware{}, errors.New("body_limit: size must be positive")
		}
		return resolved.Middleware{Type: resolved.MWBodyLimit, BodyLimitBytes: n}, nil

	case *requestIDMW:
		return resolved.Middleware{
			Type:                resolved.MWRequestID,
			RequestIDHeader:     m.header,
			RequestIDFromHeader: m.fromHeader,
		}, nil

	case *securityHeadersMW:
		hstsHeader := ""
		if m.hsts != "" {
			d, err := parseDuration(m.hsts)
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

	case *allowIPsMW:
		canon, err := resolveCIDRs(m.cidrs)
		if err != nil {
			return resolved.Middleware{}, fmt.Errorf("allow_ips: %w", err)
		}
		return resolved.Middleware{Type: resolved.MWAllowIPs, IPCIDRs: canon}, nil

	case *denyIPsMW:
		canon, err := resolveCIDRs(m.cidrs)
		if err != nil {
			return resolved.Middleware{}, fmt.Errorf("deny_ips: %w", err)
		}
		return resolved.Middleware{Type: resolved.MWDenyIPs, IPCIDRs: canon}, nil

	case *basicAuthMW:
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

	case *corsMW:
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
			d, err := parseDuration(m.maxAge)
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

	default:
		return resolved.Middleware{}, fmt.Errorf("unknown middleware type %T", mw)
	}
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
	d, err := parseDurationOr(s.GracePeriod, 30*time.Second)
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

func parseDurationOr(s string, fallback time.Duration) (time.Duration, error) {
	if s == "" {
		return fallback, nil
	}
	return parseDuration(s)
}

// parseDuration accepts every unit Go's time.ParseDuration accepts (ns, us,
// ms, s, m, h) plus "d" for days (24h) and "w" for weeks (7d). Days and
// weeks are de-sugared by string-rewriting before falling through to the
// stdlib parser, so they compose with the other units ("1w2d" works).
func parseDuration(s string) (time.Duration, error) {
	normalized, err := expandDayWeekUnits(s)
	if err != nil {
		return 0, err
	}
	d, err := time.ParseDuration(normalized)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", s, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("duration %q is negative", s)
	}
	return d, nil
}

// expandDayWeekUnits rewrites Nd → N*24h and Nw → N*168h. The rewrite is
// purely textual — it requires the number to immediately precede the unit
// suffix and falls through to a plain ParseDuration error otherwise.
func expandDayWeekUnits(s string) (string, error) {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		c := s[i]
		// Capture a number prefix (with optional sign + fractional part).
		if c == '-' || c == '+' || (c >= '0' && c <= '9') {
			j := i
			if c == '-' || c == '+' {
				j++
			}
			for j < len(s) && ((s[j] >= '0' && s[j] <= '9') || s[j] == '.') {
				j++
			}
			if j == i || (j == i+1 && (s[i] == '-' || s[i] == '+')) {
				// Not actually a number; fall through to default copy.
				b.WriteByte(c)
				i++
				continue
			}
			// Check the unit suffix.
			if j < len(s) && (s[j] == 'd' || s[j] == 'w') {
				num := s[i:j]
				hours := 24
				if s[j] == 'w' {
					hours = 24 * 7
				}
				n, err := strconv.ParseFloat(num, 64)
				if err != nil {
					return "", fmt.Errorf("duration %q: invalid number %q: %w", s, num, err)
				}
				h := n * float64(hours)
				// Emit as "<h>h" so time.ParseDuration handles it.
				b.WriteString(strconv.FormatFloat(h, 'f', -1, 64))
				b.WriteByte('h')
				i = j + 1
				continue
			}
			// No d/w suffix — copy the captured number verbatim.
			b.WriteString(s[i:j])
			i = j
			continue
		}
		b.WriteByte(c)
		i++
	}
	return b.String(), nil
}

// parseRate parses a rate of the form "N/unit" into requests per second.
// Supported units: s, sec, second; m, min, minute; h, hr, hour.
func parseRate(s string) (float64, error) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return 0, fmt.Errorf("rate %q must be N/unit", s)
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	if err != nil {
		return 0, fmt.Errorf("rate %q: invalid count: %w", s, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("rate %q: count must be positive", s)
	}
	switch strings.ToLower(strings.TrimSpace(parts[1])) {
	case "s", "sec", "second", "seconds":
		return n, nil
	case "m", "min", "minute", "minutes":
		return n / 60, nil
	case "h", "hr", "hour", "hours":
		return n / 3600, nil
	default:
		return 0, fmt.Errorf("rate %q: unknown unit %q", s, parts[1])
	}
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

// parseSize parses a byte size like "1MB", "512KiB", or "256" into a count
// of bytes. Suffixes are case-insensitive. Decimal (KB/MB/GB) and binary
// (KiB/MiB/GiB) units are both accepted. Decimal units use powers of 1000;
// binary units use powers of 1024.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("size: empty")
	}
	// Walk to the boundary between digits/dot and unit.
	i := 0
	for i < len(s) {
		c := s[i]
		if (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '+' {
			i++
			continue
		}
		break
	}
	numStr := strings.TrimSpace(s[:i])
	unit := strings.ToLower(strings.TrimSpace(s[i:]))
	if numStr == "" {
		return 0, fmt.Errorf("size %q: missing number", s)
	}
	n, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: invalid number: %w", s, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("size %q: negative", s)
	}
	var mult float64
	switch unit {
	case "", "b":
		mult = 1
	case "k", "kb":
		mult = 1000
	case "m", "mb":
		mult = 1000 * 1000
	case "g", "gb":
		mult = 1000 * 1000 * 1000
	case "kib":
		mult = 1024
	case "mib":
		mult = 1024 * 1024
	case "gib":
		mult = 1024 * 1024 * 1024
	default:
		return 0, fmt.Errorf("size %q: unknown unit %q", s, unit)
	}
	return int64(n * mult), nil
}
