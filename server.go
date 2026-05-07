package statute

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"golang.org/x/crypto/acme/autocert"

	"github.com/kjanat/statute/resolved"
)

type server struct {
	cfg *resolved.Config

	listeners     []*http.Server // content + redirect listeners
	metricsServer *http.Server
	http3Servers  []*http3Listener

	pools map[string]*poolHandler

	stats *stats

	autocertMgr     *autocert.Manager
	dns01Managers   map[string]*dns01Manager // keyed by listener address
	tracingShutdown func(context.Context) error

	mu      sync.Mutex
	started bool
}

func newServer(cfg *resolved.Config) (*server, error) {
	s := &server{
		cfg:   cfg,
		pools: make(map[string]*poolHandler, len(cfg.Upstreams)),
		stats: newStats(),
	}

	mgr, err := buildAutocertManager(cfg.Listeners)
	if err != nil {
		return nil, err
	}
	s.autocertMgr = mgr

	s.dns01Managers = make(map[string]*dns01Manager)
	for _, l := range cfg.Listeners {
		if l.AutoTLS == nil || l.AutoTLS.DNS01 == nil {
			continue
		}
		dm, err := newDNS01Manager(l.AutoTLS)
		if err != nil {
			return nil, fmt.Errorf("dns01 manager %s: %w", l.Addr, err)
		}
		s.dns01Managers[l.Addr] = dm
	}

	tracingShutdown, err := initTracing(cfg.Observability.Tracing)
	if err != nil {
		return nil, fmt.Errorf("tracing: %w", err)
	}
	s.tracingShutdown = tracingShutdown

	for name, p := range cfg.Upstreams {
		ph, err := newPoolHandler(p)
		if err != nil {
			return nil, fmt.Errorf("upstream %q: %w", name, err)
		}
		s.pools[name] = ph
	}

	mux := s.buildRouter()

	for _, l := range cfg.Listeners {
		hs, err := s.buildHTTPServer(l, mux)
		if err != nil {
			return nil, fmt.Errorf("listener %s: %w", l.Addr, err)
		}
		s.listeners = append(s.listeners, hs)

		if l.HTTP3Addr != "" {
			h3, err := s.buildHTTP3Server(l, mux)
			if err != nil {
				return nil, fmt.Errorf("listener %s http3: %w", l.Addr, err)
			}
			s.http3Servers = append(s.http3Servers, h3)
		}
	}

	if cfg.Observability.Metrics.Enabled {
		s.metricsServer = s.buildMetricsServer(cfg.Observability.Metrics)
	}

	return s, nil
}

func (s *server) buildHTTPServer(l *resolved.Listener, content http.Handler) (*http.Server, error) {
	var handler http.Handler
	if l.Redirect != "" {
		handler = redirectHandler(l.Redirect)
	} else {
		handler = content
	}

	// When AutoTLS is configured anywhere, the plain-HTTP listener must serve
	// /.well-known/acme-challenge/* so HTTP-01 can complete. autocert.HTTPHandler
	// transparently passes other paths through to the wrapped handler.
	if l.Scheme == "http" && s.autocertMgr != nil {
		handler = s.autocertMgr.HTTPHandler(handler)
	}

	// When HTTP/3 is enabled on a sibling listener, advertise it via Alt-Svc
	// so compatible clients upgrade. Browsers need this header on the HTTPS
	// response that introduces the origin.
	if l.Scheme == "https" && l.HTTP3Addr != "" {
		handler = altSvcHandler(l.HTTP3Addr, handler)
	}

	if s.cfg.Observability.AccessLog.Enabled {
		handler = accessLogMiddleware(s.cfg.Observability.AccessLog, handler)
	}
	handler = metricsMiddleware(s.stats, handler)

	// Tracing wraps last among observability so spans cover the full request
	// — including access-log writes and metric updates — and so downstream
	// handlers can read the active span from the context.
	if s.cfg.Observability.Tracing.Enabled {
		handler = tracingMiddleware(s.cfg.Observability.Tracing, handler)
	}

	// Tag the request context so downstream handlers know they can trust
	// Cloudflare-injected headers. Must wrap last so the tag is present
	// when middleware (access log, rate limit) inspects the request.
	if l.BehindCloudflare {
		handler = behindCloudflareMiddleware(handler)
	}

	hs := &http.Server{
		Addr:              l.Addr,
		Handler:           handler,
		ReadHeaderTimeout: s.cfg.Defaults.ReadHeaderTimeout,
		ReadTimeout:       s.cfg.Defaults.ReadTimeout,
		WriteTimeout:      s.cfg.Defaults.WriteTimeout,
		IdleTimeout:       s.cfg.Defaults.IdleTimeout,
		MaxHeaderBytes:    s.cfg.Defaults.MaxHeaderBytes,
	}

	if l.Scheme == "https" && l.Redirect == "" {
		switch {
		case l.AutoTLS != nil && l.AutoTLS.DNS01 != nil:
			dm := s.dns01Managers[l.Addr]
			if dm == nil {
				return nil, errors.New("auto_tls: dns01 manager not initialised")
			}
			hs.TLSConfig = dns01TLSConfig(dm, l.EnableHTTP2)
		case l.AutoTLS != nil:
			if s.autocertMgr == nil {
				return nil, errors.New("auto_tls: manager not initialised")
			}
			hs.TLSConfig = autocertTLSConfig(s.autocertMgr, l.EnableHTTP2, l.BehindCloudflare)
		case l.StaticTLS != nil:
			// TLS config left to ServeTLS; cert/key paths are passed at start.
		default:
			return nil, errors.New("https listener has no TLS material")
		}
	}
	return hs, nil
}

func (s *server) buildMetricsServer(m resolved.Metrics) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc(m.Path, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		s.stats.WritePrometheus(w)
	})
	registerPprof(mux)
	return &http.Server{
		Addr:              m.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

func (s *server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return errors.New("already started")
	}
	for addr, dm := range s.dns01Managers {
		if err := dm.start(); err != nil {
			return fmt.Errorf("dns01 manager %s: %w", addr, err)
		}
	}
	for _, hs := range s.listeners {
		hs := hs
		ln, err := net.Listen("tcp", hs.Addr)
		if err != nil {
			return fmt.Errorf("listen %s: %w", hs.Addr, err)
		}
		l, _ := findListener(s.cfg.Listeners, hs.Addr)
		go func() {
			switch {
			case l != nil && l.Scheme == "https" && l.Redirect == "" && l.StaticTLS != nil:
				_ = hs.ServeTLS(ln, l.StaticTLS.CertFile, l.StaticTLS.KeyFile)
			case l != nil && l.Scheme == "https" && l.Redirect == "" && l.AutoTLS != nil:
				// TLSConfig (set on the http.Server) carries the cert source
				// — autocert.Manager.GetCertificate or our dns01Manager
				// equivalent. ServeTLS with empty paths uses it.
				_ = hs.ServeTLS(ln, "", "")
			default:
				_ = hs.Serve(ln)
			}
		}()
	}
	for _, h3 := range s.http3Servers {
		h3 := h3
		go func() { _ = h3.Serve() }()
	}
	if s.metricsServer != nil {
		ms := s.metricsServer
		ln, err := net.Listen("tcp", ms.Addr)
		if err != nil {
			return fmt.Errorf("metrics listen %s: %w", ms.Addr, err)
		}
		go func() { _ = ms.Serve(ln) }()
	}
	s.started = true
	return nil
}

func (s *server) Shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Shutdown.GracePeriod)
	defer cancel()

	var wg sync.WaitGroup
	errs := make(chan error, len(s.listeners)+len(s.http3Servers)+1)

	for _, hs := range s.listeners {
		wg.Add(1)
		hs := hs
		go func() {
			defer wg.Done()
			if err := hs.Shutdown(ctx); err != nil {
				errs <- err
			}
		}()
	}
	if s.metricsServer != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.metricsServer.Shutdown(ctx); err != nil {
				errs <- err
			}
		}()
	}
	for _, h3 := range s.http3Servers {
		wg.Add(1)
		h3 := h3
		go func() {
			defer wg.Done()
			if err := h3.Shutdown(ctx); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()

	// Stop health checkers after listeners drain so probes do not race
	// shutdown of the metrics server.
	for _, ph := range s.pools {
		ph.shutdown()
	}
	for _, dm := range s.dns01Managers {
		dm.stop()
	}

	// Flush pending spans last so traces produced during shutdown still ship.
	if s.tracingShutdown != nil {
		if err := s.tracingShutdown(ctx); err != nil {
			errs <- err
		}
	}

	close(errs)
	return joinErrors(errs)
}

func findListener(ls []*resolved.Listener, addr string) (*resolved.Listener, bool) {
	for _, l := range ls {
		if l.Addr == addr {
			return l, true
		}
	}
	return nil, false
}

func joinErrors(ch <-chan error) error {
	var collected []error
	for e := range ch {
		collected = append(collected, e)
	}
	if len(collected) == 0 {
		return nil
	}
	return errors.Join(collected...)
}

// buildRouter returns an http.Handler that dispatches to the matching route
// in declaration order.
func (s *server) buildRouter() http.Handler {
	type compiled struct {
		route   *resolved.Route
		handler http.Handler
	}
	routes := make([]compiled, 0, len(s.cfg.Routes))
	for _, r := range s.cfg.Routes {
		var base http.Handler
		switch {
		case r.Upstream != nil:
			base = s.pools[r.Upstream.Name]
		case r.StaticDir != "":
			base = http.FileServer(http.Dir(r.StaticDir))
			base = stripPrefix(r.Pattern, base)
		}
		h := wrapMiddleware(r.Middleware, base)
		routes = append(routes, compiled{route: r, handler: h})
	}

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		host := stripPort(req.Host)
		for _, c := range routes {
			if c.route.Host != "" && c.route.Host != host {
				continue
			}
			if !matchPattern(c.route.Pattern, req.URL.Path) {
				continue
			}
			c.handler.ServeHTTP(w, req)
			return
		}
		http.NotFound(w, req)
	})
}

func stripPort(hostport string) string {
	if i := strings.LastIndex(hostport, ":"); i >= 0 {
		// IPv6 hosts will be in brackets; conservative split only when no bracket.
		if !strings.Contains(hostport[:i], "]") {
			return hostport[:i]
		}
	}
	return hostport
}

// matchPattern matches a path against a pattern. A trailing /* matches any
// suffix; otherwise the match is exact.
func matchPattern(pattern, path string) bool {
	if strings.HasSuffix(pattern, "/*") {
		prefix := strings.TrimSuffix(pattern, "/*")
		if prefix == "" {
			return true
		}
		return strings.HasPrefix(path, prefix+"/") || path == prefix
	}
	return pattern == path
}

// stripPrefix strips the static-route prefix so the FileServer sees a clean path.
func stripPrefix(pattern string, h http.Handler) http.Handler {
	prefix := strings.TrimSuffix(pattern, "/*")
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix == "" {
		return h
	}
	return http.StripPrefix(prefix, h)
}

func redirectHandler(scheme string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := stripPort(r.Host)
		target := scheme + "://" + host + r.URL.RequestURI()
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	})
}

// poolHandler proxies requests to an upstream pool. It owns a per-backend
// reverse proxy, a strategy-driven picker, and an active health checker that
// runs in the background while the pool is live.
type poolHandler struct {
	pool      *resolved.Pool
	primary   []*backendState
	backup    []*backendState
	transport *http.Transport
	picker    picker
	hc        *healthChecker
}

func newPoolHandler(p *resolved.Pool) (*poolHandler, error) {
	transport := &http.Transport{
		MaxIdleConnsPerHost: p.Transport.MaxIdleConnsPerHost,
		IdleConnTimeout:     p.Transport.IdleConnTimeout,
		TLSHandshakeTimeout: p.Transport.TLSHandshakeTimeout,
		DialContext: (&net.Dialer{
			Timeout:   p.Transport.DialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2: true,
	}

	ph := &poolHandler{
		pool:      p,
		transport: transport,
		picker:    newPicker(p.Strategy),
	}

	for i := range p.Backends {
		b := &p.Backends[i]
		target, err := backendURL(b)
		if err != nil {
			return nil, fmt.Errorf("backend %s: %w", b.Address, err)
		}
		bs := &backendState{backend: b}
		// Backends start healthy; the prober demotes them as it observes failures.
		bs.markHealthy(true)
		bs.rp = newBackendProxy(target, transport)
		if b.Backup {
			ph.backup = append(ph.backup, bs)
		} else {
			ph.primary = append(ph.primary, bs)
		}
	}

	all := append(append([]*backendState{}, ph.primary...), ph.backup...)
	ph.hc = newHealthChecker(p.HealthCheck, all)
	ph.hc.start()
	return ph, nil
}

func newBackendProxy(target *url.URL, transport *http.Transport) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
			// Inject W3C trace context so the upstream sees traceparent /
			// tracestate headers and joins the same trace. Safe to call
			// regardless of whether tracing is configured: when no provider
			// is registered, the propagator is a no-op.
			otel.GetTextMapPropagator().Inject(pr.Out.Context(), propagation.HeaderCarrier(pr.Out.Header))
		},
		Transport: transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		},
	}
}

func backendURL(b *resolved.Backend) (*url.URL, error) {
	addr := b.Address
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	return url.Parse(addr)
}

// candidates returns the active backend set: healthy primaries when any are
// healthy, else healthy backups, else all primaries (degraded mode — better
// to attempt a probably-failing dial than to flat-out 503).
func (ph *poolHandler) candidates() []*backendState {
	if c := healthy(ph.primary); len(c) > 0 {
		return c
	}
	if c := healthy(ph.backup); len(c) > 0 {
		return c
	}
	return ph.primary
}

func (ph *poolHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	candidates := ph.candidates()
	if len(candidates) == 0 {
		http.Error(w, "no backends available", http.StatusServiceUnavailable)
		return
	}
	bs := ph.picker.pick(candidates, clientIP(r))
	if bs == nil {
		http.Error(w, "no backends available", http.StatusServiceUnavailable)
		return
	}
	bs.inFlight.Add(1)
	defer bs.inFlight.Add(-1)
	bs.rp.ServeHTTP(w, r)
}

// shutdown stops the health checker and idle connections.
func (ph *poolHandler) shutdown() {
	if ph.hc != nil {
		ph.hc.stop()
	}
	ph.transport.CloseIdleConnections()
}

// wrapMiddleware wraps the base handler with each middleware in declaration
// order. The first middleware in the list runs outermost.
func wrapMiddleware(mws []resolved.Middleware, base http.Handler) http.Handler {
	if base == nil {
		base = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "no handler configured for route", http.StatusInternalServerError)
		})
	}
	h := base
	for i := len(mws) - 1; i >= 0; i-- {
		h = applyMiddleware(mws[i], h)
	}
	return h
}

func applyMiddleware(m resolved.Middleware, next http.Handler) http.Handler {
	switch m.Type {
	case resolved.MWTimeout:
		return http.TimeoutHandler(next, m.Timeout, "request timed out")
	case resolved.MWRateLimit:
		return rateLimitHandler(m, next)
	case resolved.MWRetry:
		return retryHandler(m, next)
	case resolved.MWCache:
		return cacheHandler(m, next)
	case resolved.MWCompress:
		return compressHandler(m.CompressAlgos, next)
	case resolved.MWETag:
		return etagHandler(next)
	default:
		return next
	}
}
