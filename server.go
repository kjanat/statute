package statute

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"golang.org/x/crypto/acme/autocert"

	"statute.kjanat.dev/resolved"
)

type server struct {
	cfg *resolved.Config

	listeners     []*http.Server // content + redirect listeners
	metricsServer *http.Server
	http3Servers  []*http3Listener

	pools map[string]*poolHandler

	// docker is the label-discovery provider; nil unless configured.
	// dynamic is the current generation of label-derived routes, consulted
	// after static routes so labels can never shadow compiled config.
	docker  *dockerProvider
	dynamic atomic.Pointer[dynamicTable]

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

	if err := s.initDNS01Managers(cfg.Listeners); err != nil {
		return nil, err
	}

	tracingShutdown, err := initTracing(cfg.Observability.Tracing)
	if err != nil {
		return nil, fmt.Errorf("tracing: %w", err)
	}
	s.tracingShutdown = tracingShutdown
	// If a later init step fails the caller never gets a *server to call
	// Shutdown on, so flush the tracing provider here on the error path.
	ok := false
	defer func() {
		if !ok && tracingShutdown != nil {
			_ = tracingShutdown(context.Background())
		}
	}()

	if err := s.initPools(cfg.Upstreams); err != nil {
		return nil, err
	}
	if err := s.initDocker(cfg.Docker); err != nil {
		return nil, err
	}
	if err := s.initListeners(cfg.Listeners, s.buildRouter()); err != nil {
		return nil, err
	}

	if cfg.Observability.Metrics.Enabled {
		s.metricsServer = s.buildMetricsServer(cfg.Observability.Metrics)
	}

	ok = true
	return s, nil
}

// initDNS01Managers builds a dns01Manager for every listener configured
// with Cloudflare DNS-01, keyed by listener address.
func (s *server) initDNS01Managers(listeners []*resolved.Listener) error {
	s.dns01Managers = make(map[string]*dns01Manager)
	for _, l := range listeners {
		if l.AutoTLS == nil || l.AutoTLS.DNS01 == nil {
			continue
		}
		dm, err := newDNS01Manager(l.AutoTLS)
		if err != nil {
			return fmt.Errorf("dns01 manager %s: %w", l.Addr, err)
		}
		s.dns01Managers[l.Addr] = dm
	}
	return nil
}

// initDocker constructs the label-discovery provider when configured.
func (s *server) initDocker(cfg *resolved.Docker) error {
	if cfg == nil {
		return nil
	}
	dp, err := newDockerProvider(cfg, s)
	if err != nil {
		return fmt.Errorf("docker: %w", err)
	}
	s.docker = dp
	return nil
}

// startDocker runs the provider's initial sync before listeners open so
// the first request already sees label-derived routes.
func (s *server) startDocker() error {
	if s.docker == nil {
		return nil
	}
	if err := s.docker.start(); err != nil {
		return fmt.Errorf("docker: %w", err)
	}
	return nil
}

// rollbackDockerUnlessStarted undoes startDocker when a later startup step
// failed. Deferred from Start; a completed Start sets started first.
func (s *server) rollbackDockerUnlessStarted() {
	if !s.started {
		s.shutdownDocker()
	}
}

// shutdownDocker stops the provider before its pools so no reconcile swaps
// in a fresh generation mid-teardown, then retires the current generation.
func (s *server) shutdownDocker() {
	if s.docker == nil {
		return
	}
	s.docker.stop()
	if t := s.dynamic.Load(); t != nil {
		for _, ph := range t.pools {
			ph.shutdown()
		}
	}
}

// initPools builds a poolHandler for every resolved upstream.
func (s *server) initPools(upstreams map[string]*resolved.Pool) error {
	for name, p := range upstreams {
		ph, err := newPoolHandler(p)
		if err != nil {
			return fmt.Errorf("upstream %q: %w", name, err)
		}
		s.pools[name] = ph
	}
	return nil
}

// initListeners builds the HTTP (and, where configured, HTTP/3) servers
// for every resolved listener, sharing the given handler.
func (s *server) initListeners(listeners []*resolved.Listener, mux http.Handler) error {
	for _, l := range listeners {
		hs, err := s.buildHTTPServer(l, mux)
		if err != nil {
			return fmt.Errorf("listener %s: %w", l.Addr, err)
		}
		s.listeners = append(s.listeners, hs)

		if l.HTTP3Addr == "" {
			continue
		}
		h3, err := s.buildHTTP3Server(l, mux)
		if err != nil {
			return fmt.Errorf("listener %s http3: %w", l.Addr, err)
		}
		s.http3Servers = append(s.http3Servers, h3)
	}
	return nil
}

func (s *server) buildHTTPServer(l *resolved.Listener, content http.Handler) (*http.Server, error) {
	hs := &http.Server{
		Addr:              l.Addr,
		Handler:           s.buildListenerHandler(l, content),
		ReadHeaderTimeout: s.cfg.Defaults.ReadHeaderTimeout,
		ReadTimeout:       s.cfg.Defaults.ReadTimeout,
		WriteTimeout:      s.cfg.Defaults.WriteTimeout,
		IdleTimeout:       s.cfg.Defaults.IdleTimeout,
		MaxHeaderBytes:    s.cfg.Defaults.MaxHeaderBytes,
	}

	if l.Scheme == schemeHTTPS && l.Redirect == "" {
		if err := s.applyListenerTLS(hs, l); err != nil {
			return nil, err
		}
	}
	return hs, nil
}

// buildListenerHandler assembles the middleware chain for a listener. The
// wrapping order is deliberate: each block comments why it must sit where it
// does relative to the others.
func (s *server) buildListenerHandler(l *resolved.Listener, content http.Handler) http.Handler {
	var handler http.Handler
	if l.Redirect != "" {
		handler = redirectHandler(l.Redirect)
	} else {
		handler = content
	}

	// When AutoTLS is configured anywhere, the plain-HTTP listener must serve
	// /.well-known/acme-challenge/* so HTTP-01 can complete. autocert.HTTPHandler
	// transparently passes other paths through to the wrapped handler.
	if l.Scheme == schemeHTTP && s.autocertMgr != nil {
		handler = s.autocertMgr.HTTPHandler(handler)
	}

	// When HTTP/3 is enabled on a sibling listener, advertise it via Alt-Svc
	// so compatible clients upgrade. Browsers need this header on the HTTPS
	// response that introduces the origin.
	if l.Scheme == schemeHTTPS && l.HTTP3Addr != "" {
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
	return handler
}

// applyListenerTLS selects the TLS source for an HTTPS content listener and
// installs it on hs. StaticTLS is intentionally a no-op here: its cert/key
// paths are passed to ServeTLS at start time instead.
func (s *server) applyListenerTLS(hs *http.Server, l *resolved.Listener) error {
	switch {
	case l.AutoTLS != nil && l.AutoTLS.DNS01 != nil:
		dm := s.dns01Managers[l.Addr]
		if dm == nil {
			return errors.New("auto_tls: dns01 manager not initialised")
		}
		hs.TLSConfig = dns01TLSConfig(dm, l.EnableHTTP2)
	case l.AutoTLS != nil:
		if s.autocertMgr == nil {
			return errors.New("auto_tls: manager not initialised")
		}
		hs.TLSConfig = autocertTLSConfig(s.autocertMgr, l.EnableHTTP2, l.BehindCloudflare)
	case l.StaticTLS != nil:
		// TLS config left to ServeTLS; cert/key paths are passed at start.
	default:
		return errors.New("https listener has no TLS material")
	}
	return nil
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

// Start opens all configured listeners and begins serving. Calling it
// after the server has already started returns an error.
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
	if err := s.startDocker(); err != nil {
		return err
	}
	// If a later startup step fails, stop the provider again so a
	// failed Start does not leak its reconcile loop and pools.
	defer s.rollbackDockerUnlessStarted()
	for _, hs := range s.listeners {
		ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", hs.Addr)
		if err != nil {
			return fmt.Errorf("listen %s: %w", hs.Addr, err)
		}
		l, _ := findListener(s.cfg.Listeners, hs.Addr)
		go serveListener(hs, l, ln)
	}
	for _, h3 := range s.http3Servers {
		go func() { _ = h3.Serve() }()
	}
	if s.metricsServer != nil {
		ms := s.metricsServer
		ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", ms.Addr)
		if err != nil {
			return fmt.Errorf("metrics listen %s: %w", ms.Addr, err)
		}
		go func() { _ = ms.Serve(ln) }()
	}
	s.started = true
	return nil
}

// serveListener runs hs on ln, picking ServeTLS vs Serve based on the
// resolved listener's TLS material. For AutoTLS the cert source lives on
// hs.TLSConfig (autocert.Manager.GetCertificate or the dns01Manager
// equivalent), so ServeTLS is called with empty paths.
func serveListener(hs *http.Server, l *resolved.Listener, ln net.Listener) {
	isContentHTTPS := l != nil && l.Scheme == schemeHTTPS && l.Redirect == ""
	switch {
	case isContentHTTPS && l.StaticTLS != nil:
		_ = hs.ServeTLS(ln, l.StaticTLS.CertFile, l.StaticTLS.KeyFile)
	case isContentHTTPS && l.AutoTLS != nil:
		_ = hs.ServeTLS(ln, "", "")
	default:
		_ = hs.Serve(ln)
	}
}

// goShutdown runs fn(ctx) in a WaitGroup-tracked goroutine, forwarding any
// error to errs. errs must be buffered enough to hold one error per call so
// the sends never block before wg.Wait returns.
func goShutdown(ctx context.Context, wg *sync.WaitGroup, errs chan<- error, fn func(context.Context) error) {
	wg.Go(func() {
		if err := fn(ctx); err != nil {
			errs <- err
		}
	})
}

// Shutdown gracefully drains and stops the server within the configured
// shutdown grace period.
func (s *server) Shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Shutdown.GracePeriod)
	defer cancel()

	var wg sync.WaitGroup
	errs := make(chan error, len(s.listeners)+len(s.http3Servers)+1)

	for _, hs := range s.listeners {
		goShutdown(ctx, &wg, errs, hs.Shutdown)
	}
	if s.metricsServer != nil {
		goShutdown(ctx, &wg, errs, s.metricsServer.Shutdown)
	}
	for _, h3 := range s.http3Servers {
		goShutdown(ctx, &wg, errs, h3.Shutdown)
	}
	wg.Wait()

	s.shutdownDocker()

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

// compiledRoute pairs a resolved route with its ready-to-serve handler
// chain. Both static routes and docker label-derived routes compile to it.
type compiledRoute struct {
	route   *resolved.Route
	handler http.Handler
}

// findHandler returns the first route matching host and path, in slice
// order, or nil. Host comparison is case-insensitive per RFC 9110.
func findHandler(routes []compiledRoute, host, path string) http.Handler {
	for _, c := range routes {
		if c.route.Host != "" && !strings.EqualFold(c.route.Host, host) {
			continue
		}
		if !matchPattern(c.route.Pattern, path) {
			continue
		}
		return c.handler
	}
	return nil
}

// buildRouter returns an http.Handler that dispatches to the matching
// static route in declaration order, then to the docker provider's dynamic
// routes when one is configured.
func (s *server) buildRouter() http.Handler {
	static := make([]compiledRoute, 0, len(s.cfg.Routes))
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
		static = append(static, compiledRoute{route: r, handler: h})
	}

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		host := stripPort(req.Host)
		if h := findHandler(static, host, req.URL.Path); h != nil {
			h.ServeHTTP(w, req)
			return
		}
		if t := s.dynamic.Load(); t != nil {
			if h := findHandler(t.routes, host, req.URL.Path); h != nil {
				h.ServeHTTP(w, req)
				return
			}
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
	if before, ok := strings.CutSuffix(pattern, "/*"); ok {
		prefix := before
		if prefix == "" {
			return true
		}
		return strings.HasPrefix(path, prefix+"/") || path == prefix
	}
	return pattern == path
}

// stripPrefix strips the static-route prefix so the FileServer sees a clean
// path. Only a trailing-wildcard pattern names a directory prefix: /static/*
// serving ./public maps /static/css/app.css to ./public/css/app.css. An exact
// pattern names one file below the served directory, so its path is passed
// through untouched and Match("/robots.txt").Serve("./public") serves
// ./public/robots.txt rather than the directory root.
func stripPrefix(pattern string, h http.Handler) http.Handler {
	prefix, wildcard := strings.CutSuffix(pattern, "/*")
	if !wildcard {
		return h
	}
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
		http.Redirect(w, r, target, http.StatusMovedPermanently) //nolint:gosec // G710: intentional same-host HTTP→HTTPS upgrade; redirecting to the requested host+path is the required behavior
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

// ServeHTTP proxies the request to a healthy backend in the pool,
// responding 503 when no backends are available.
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
	for _, mw := range slices.Backward(mws) {
		h = applyMiddleware(mw, h)
	}
	return h
}

// middlewareBuilders maps each resolved middleware type to the constructor
// that wraps the next handler. Constructors with a non-uniform signature
// (timeout, compress, etag) are bridged by small adapters below. An unknown
// type falls through applyMiddleware to a pass-through.
var middlewareBuilders = map[resolved.MiddlewareType]func(resolved.Middleware, http.Handler) http.Handler{
	resolved.MWTimeout:         buildTimeout,
	resolved.MWRateLimit:       rateLimitHandler,
	resolved.MWRetry:           retryHandler,
	resolved.MWCache:           cacheHandler,
	resolved.MWCompress:        buildCompress,
	resolved.MWETag:            buildETag,
	resolved.MWBodyLimit:       bodyLimitHandler,
	resolved.MWRequestID:       requestIDHandler,
	resolved.MWSecurityHeaders: securityHeadersHandler,
	resolved.MWCORS:            corsHandler,
	resolved.MWBasicAuth:       basicAuthHandler,
	resolved.MWAllowIPs:        allowIPsHandler,
	resolved.MWDenyIPs:         denyIPsHandler,
}

// buildTimeout adapts http.TimeoutHandler to the middlewareBuilders signature.
func buildTimeout(m resolved.Middleware, next http.Handler) http.Handler {
	return http.TimeoutHandler(next, m.Timeout, "request timed out")
}

// buildCompress adapts compressHandler to the middlewareBuilders signature.
func buildCompress(m resolved.Middleware, next http.Handler) http.Handler {
	return compressHandler(m.CompressAlgos, next)
}

// buildETag adapts etagHandler to the middlewareBuilders signature.
func buildETag(_ resolved.Middleware, next http.Handler) http.Handler {
	return etagHandler(next)
}

func applyMiddleware(m resolved.Middleware, next http.Handler) http.Handler {
	if build, ok := middlewareBuilders[m.Type]; ok {
		return build(m, next)
	}
	return next
}
