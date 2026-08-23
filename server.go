package statute

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
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
	acmeManagers    map[*resolved.AutoTLS]*acmeManager // keyed by resolved source
	certRouters     map[string]*certRouter             // keyed by listener address
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

	if err := s.initACMEManagers(cfg.Listeners); err != nil {
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

// initACMEManagers builds an in-tree acmeManager for every AutoTLS source
// with a pinned challenge — DNS-01 via Cloudflare, or explicit HTTP-01 —
// keyed by the resolved source so each listener's cert router can find the
// manager for exactly the source it is dispatching to. Automatic sources
// stay on the shared autocert manager.
func (s *server) initACMEManagers(listeners []*resolved.Listener) error {
	s.acmeManagers = make(map[*resolved.AutoTLS]*acmeManager)
	for _, l := range listeners {
		for _, a := range l.AutoTLSSources {
			var (
				m   *acmeManager
				err error
			)
			switch {
			case a.DNS01 != nil:
				m, err = newDNS01Manager(a)
			case a.Challenge == resolved.ChallengeHTTP01:
				m, err = newHTTP01Manager(a)
			default:
				continue
			}
			if err != nil {
				return fmt.Errorf("acme manager %s: %w", l.Addr, err)
			}
			s.acmeManagers[a] = m
		}
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
		// One wrapped handler per listener, shared by the TCP server and
		// the QUIC server, so HTTP/3 requests pass through exactly the
		// middleware HTTP/1.1 and HTTP/2 do — trusted-proxy policy and
		// observability included. Two transports with separately-assembled
		// chains would drift apart.
		handler := s.buildListenerHandler(l, mux)
		hs, err := s.buildHTTPServer(l, handler)
		if err != nil {
			return fmt.Errorf("listener %s: %w", l.Addr, err)
		}
		s.listeners = append(s.listeners, hs)

		if l.HTTP3Addr == "" {
			continue
		}
		h3, err := s.buildHTTP3Server(l, handler)
		if err != nil {
			return fmt.Errorf("listener %s http3: %w", l.Addr, err)
		}
		s.http3Servers = append(s.http3Servers, h3)
	}
	return nil
}

// buildHTTPServer wires the listener's already-wrapped handler into a TCP
// http.Server; initListeners builds that handler once and shares it with
// the listener's QUIC server.
func (s *server) buildHTTPServer(l *resolved.Listener, handler http.Handler) (*http.Server, error) {
	hs := &http.Server{
		Addr:              l.Addr,
		Handler:           handler,
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

	// When AutoTLS is configured anywhere, the plain-HTTP listener must
	// serve /.well-known/acme-challenge/* so HTTP-01 can complete.
	if l.Scheme == schemeHTTP {
		handler = s.wrapACMEChallenges(handler)
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
	// The per-peer trust policy wraps outermost for the same reason; when
	// both are configured, clientIP consults this policy alone.
	if len(l.TrustedProxies) > 0 {
		handler = trustedProxyMiddleware(l, handler)
	}
	return handler
}

// warmACMEManagers eagerly issues missing certificates for one warm-up
// phase. afterListeners false selects the managers whose CA validation
// needs no local listener (DNS-01) — they warm synchronously before the
// listeners open, keeping the first handshake fast. afterListeners true
// selects the HTTP-01 managers, warmed through warmAsync once the plain
// HTTP listeners are serving and can answer the CA's token fetch; the
// manager tracks that goroutine so Shutdown waits for it.
func (s *server) warmACMEManagers(afterListeners bool) {
	for _, m := range s.acmeManagers {
		if m.warmsAfterListeners() != afterListeners {
			continue
		}
		if afterListeners {
			m.warmAsync()
		} else {
			m.warm()
		}
	}
}

// wrapACMEChallenges layers every HTTP-01 challenge responder over next on
// a plain HTTP listener: the shared autocert manager's handler covers
// automatic sources, and each pinned HTTP-01 manager serves its own token
// table. All of them pass other paths through to the wrapped handler.
func (s *server) wrapACMEChallenges(next http.Handler) http.Handler {
	if s.autocertMgr != nil {
		next = s.autocertMgr.HTTPHandler(next)
	}
	for _, m := range s.acmeManagers {
		next = m.wrapHTTPChallenges(next)
	}
	return next
}

// applyListenerTLS installs the listener's TLS material on hs: a cert
// router over every declared source, dispatching per handshake by SNI. The
// router is also stored by address so the listener's HTTP/3 server reuses
// it instead of loading static key pairs a second time.
func (s *server) applyListenerTLS(hs *http.Server, l *resolved.Listener) error {
	cr, err := s.buildCertRouter(l)
	if err != nil {
		return err
	}
	if s.certRouters == nil {
		s.certRouters = make(map[string]*certRouter)
	}
	s.certRouters[l.Addr] = cr
	hs.TLSConfig = certRouterTLSConfig(cr, l)
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
	for src, m := range s.acmeManagers {
		if err := m.start(); err != nil {
			return fmt.Errorf("%s manager (%s): %w", m.name, strings.Join(src.Domains, ", "), err)
		}
	}
	s.warmACMEManagers(false)
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
	s.warmACMEManagers(true)
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
// listener's scheme. Certificate material always flows through the cert
// router installed on hs.TLSConfig, so ServeTLS never receives file paths.
func serveListener(hs *http.Server, l *resolved.Listener, ln net.Listener) {
	if l != nil && l.Scheme == schemeHTTPS && l.Redirect == "" {
		_ = hs.ServeTLS(ln, "", "")
		return
	}
	_ = hs.Serve(ln)
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

	// Stop the ACME managers before the listeners: a warm-up still in
	// flight must be cancelled while the plain HTTP listener that serves
	// its HTTP-01 tokens is still accepting, or the CA's token fetch hits a
	// closed port and the account pays for a failed validation on the way
	// out. stop also waits for that goroutine, so nothing outlives Shutdown.
	for _, m := range s.acmeManagers {
		m.stop()
	}

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
	// clientPrefixes is the parsed form of the route's ClientIPCIDRs; empty
	// means the route matches any client.
	clientPrefixes []netip.Prefix
}

// findHandler returns the first route matching host, path, and — for routes
// constrained with ClientIPs — the verified client address, in slice order,
// or nil. Host comparison is case-insensitive per RFC 9110. The client
// address resolves lazily, once, and only when a candidate route needs it;
// a client that cannot be parsed never matches a constrained route and
// falls through like any other mismatch.
func findHandler(routes []compiledRoute, host string, req *http.Request) http.Handler {
	var clientAddr netip.Addr
	var clientResolved, clientOK bool
	for _, c := range routes {
		if c.route.Host != "" && !strings.EqualFold(c.route.Host, host) {
			continue
		}
		if !matchPattern(c.route.Pattern, req.URL.Path) {
			continue
		}
		if len(c.clientPrefixes) > 0 {
			if !clientResolved {
				clientAddr, clientOK = parseClientAddr(req)
				clientResolved = true
			}
			if !clientOK || !addrInPrefixes(clientAddr, c.clientPrefixes) {
				continue
			}
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
		case r.Redirect != nil:
			base = redirectRouteHandler(r.Redirect)
		}
		h := wrapMiddleware(r.Middleware, base)
		static = append(static, compiledRoute{
			route:          r,
			handler:        h,
			clientPrefixes: mustParsePrefixes(r.ClientIPCIDRs),
		})
	}

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		host := stripPort(req.Host)
		if h := findHandler(static, host, req); h != nil {
			h.ServeHTTP(w, req)
			return
		}
		if t := s.dynamic.Load(); t != nil {
			if h := findHandler(t.routes, host, req); h != nil {
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
	tlsCfg, err := backendTLSConfig(p.Transport)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		MaxIdleConnsPerHost: p.Transport.MaxIdleConnsPerHost,
		IdleConnTimeout:     p.Transport.IdleConnTimeout,
		TLSHandshakeTimeout: p.Transport.TLSHandshakeTimeout,
		TLSClientConfig:     tlsCfg,
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
		bs.rp = newBackendProxy(target, transport, p)
		if b.Backup {
			ph.backup = append(ph.backup, bs)
		} else {
			ph.primary = append(ph.primary, bs)
		}
	}

	all := append(append([]*backendState{}, ph.primary...), ph.backup...)
	// The prober rides the same transport as proxy traffic, so both sides
	// see one TLS verification policy that cannot drift apart. An explicit
	// Host policy carries over too; the other policies leave probes on each
	// backend's own host — a probe has no client Host to preserve.
	probeHost := ""
	if p.UpstreamHost == resolved.HostExplicit {
		probeHost = p.HostValue
	}
	ph.hc = newHealthChecker(p.HealthCheck, all, transport, probeHost)
	ph.hc.start()
	return ph, nil
}

// backendTLSConfig builds the pool's backend-verification policy, or nil
// when the pool leaves TLS at Go's defaults. CA files load here rather than
// at Resolve time, keeping Resolve pure the way listener TLS material does.
func backendTLSConfig(t resolved.Transport) (*tls.Config, error) {
	if t.ServerName == "" && len(t.RootCAFiles) == 0 && !t.InsecureSkipVerify {
		return nil, nil
	}
	cfg := &tls.Config{
		ServerName: t.ServerName,
		MinVersion: tls.VersionTLS12,
		// The escape hatch the surface API documents; lint warns on it
		// (TLS002) so it cannot slip into production unnoticed.
		InsecureSkipVerify: t.InsecureSkipVerify, //nolint:gosec // G402: explicit operator opt-out, surfaced by lint
	}
	if len(t.RootCAFiles) > 0 {
		roots := x509.NewCertPool()
		for _, f := range t.RootCAFiles {
			pemBytes, err := os.ReadFile(f) //nolint:gosec // G304: operator-configured CA path
			if err != nil {
				return nil, fmt.Errorf("root CA file: %w", err)
			}
			if !roots.AppendCertsFromPEM(pemBytes) {
				return nil, fmt.Errorf("root CA file %s: no certificates found", f)
			}
		}
		cfg.RootCAs = roots
	}
	return cfg, nil
}

func newBackendProxy(target *url.URL, transport *http.Transport, p *resolved.Pool) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// SetURL blanked Out.Host, which makes the transport derive the
			// Host header from the target URL — exactly the HostTarget
			// policy. The other policies override it.
			switch p.UpstreamHost {
			case resolved.HostTarget:
			case resolved.HostExplicit:
				pr.Out.Host = p.HostValue
			default:
				pr.Out.Host = pr.In.Host
			}
			pr.SetXForwarded()
			// SetXForwarded has just overwritten the X-Forwarded-* fields
			// from the real connection, including any a route configured on
			// purpose. Reapply those so an explicit route declaration wins
			// over the derived default, while a client still cannot spoof
			// the fields the route left alone.
			for _, op := range forwardedOpsFromContext(pr.In.Context()) {
				applyHeaderOp(pr.Out.Header, op.op, op.name, op.value)
			}
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
// order. The first middleware in the list runs outermost. Header operations
// and path rewrites are hoisted to the outside of the whole chain — see
// withHeaderMiddleware and withPathRewrite.
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
	return withHeaderMiddleware(mws, withPathRewrite(mws, h))
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
	// The header operations and the path rewrites are deliberately absent:
	// withHeaderMiddleware and withPathRewrite hoist them out of the chain so
	// a retry cannot apply them per attempt. They fall through
	// applyMiddleware as pass-throughs.
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
