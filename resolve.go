package statute

import (
	"errors"
	"fmt"
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
		Addr:        l.addr,
		Scheme:      l.scheme,
		Redirect:    l.redirect,
		EnableHTTP2: l.enableHTTP2,
		HTTP3Addr:   l.http3Addr,
	}
	if l.scheme == "https" && l.redirect == "" {
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

	default:
		return resolved.Middleware{}, fmt.Errorf("unknown middleware type %T", mw)
	}
}

func resolveObservability(o Observability) (resolved.Observability, error) {
	out := resolved.Observability{}
	if o.AccessLog != nil {
		switch al := o.AccessLog.(type) {
		case jsonLog:
			out.AccessLog = resolved.AccessLog{
				Enabled: true,
				Format:  "json",
				Writer:  al.dest.Writer(),
				Name:    al.dest.Name(),
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

func parseDuration(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", s, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("duration %q is negative", s)
	}
	return d, nil
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
