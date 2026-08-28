package statute

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
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

	"statute.kjanat.dev/internal/docker"
	"statute.kjanat.dev/resolved"
)

type server struct {
	cfg *resolved.Config

	listeners     []*http.Server // content + redirect listeners
	metricsServer *http.Server
	http3Servers  []*http3Server

	// ready flips true at Start's commit and false when Shutdown begins;
	// the health readiness handler reads it, nothing else writes it.
	ready atomic.Bool

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
	run     *serverRun

	listenTCP    func(context.Context, string, string) (net.Listener, error)
	listenPacket func(context.Context, string, string) (net.PacketConn, error)
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
		// One liveness bit per HTTP/3 pair, created before either side:
		// the Alt-Svc wrapper reads it, the serve loop writes it.
		var h3Alive *atomic.Bool
		if l.HTTP3Addr != "" {
			h3Alive = new(atomic.Bool)
		}
		// One wrapped handler per listener, shared by the TCP server and
		// the QUIC server, so HTTP/3 requests pass through exactly the
		// middleware HTTP/1.1 and HTTP/2 do — trusted-proxy policy and
		// observability included. Two transports with separately-assembled
		// chains would drift apart.
		handler := s.buildListenerHandler(l, mux, h3Alive)
		hs, err := s.buildHTTPServer(l, handler)
		if err != nil {
			return fmt.Errorf("listener %s: %w", l.Addr, err)
		}
		s.listeners = append(s.listeners, hs)

		if l.HTTP3Addr == "" {
			continue
		}
		h3, err := s.buildHTTP3Server(l, handler, h3Alive)
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
func (s *server) buildListenerHandler(l *resolved.Listener, content http.Handler, h3Alive *atomic.Bool) http.Handler {
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
		handler = altSvcHandler(l.HTTP3Addr, h3Alive, handler)
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
func warmACMERuns(runs []*acmeRun, afterListeners bool) {
	for _, run := range runs {
		if run.manager.warmsAfterListeners() != afterListeners {
			continue
		}
		if afterListeners {
			run.warmAsync()
		} else {
			run.warm()
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

// buildHealthServer builds one attempt's fresh health http.Server: an exact-path handler (no ServeMux subtrees) answering liveness at h.Path, readiness at h.Path+"/ready", 404 otherwise, with neither metrics nor pprof mounted.
func (s *server) buildHealthServer(h resolved.Health) *http.Server {
	readyPath := h.Path + "/ready"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case h.Path:
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("ok"))
		case readyPath:
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			if !s.ready.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte("not ready"))
				return
			}
			_, _ = w.Write([]byte("ok"))
		default:
			http.NotFound(w, r)
		}
	})
	return &http.Server{
		Addr:              h.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

type boundHTTP struct {
	server   *http.Server
	policy   *resolved.Listener
	listener net.Listener
}

func (b *boundHTTP) serve() {
	logServeExit("listener", b.server.Addr, serveListener(b.server, b.policy, b.listener))
}

func (b *boundHTTP) rollback() error {
	if err := b.listener.Close(); err != nil {
		return fmt.Errorf("rollback close listener %s: %w", b.server.Addr, err)
	}
	return nil
}

type boundHTTP3 struct {
	server *http3Server
	conn   net.PacketConn
}

func (b *boundHTTP3) serve() { b.server.serveLoop(b.conn) }

func (b *boundHTTP3) rollback() error {
	if err := b.conn.Close(); err != nil {
		return fmt.Errorf("rollback close http3 %s: %w", b.server.addr, err)
	}
	return nil
}

type boundMetrics struct {
	server   *http.Server
	listener net.Listener
}

func (b *boundMetrics) serve() {
	logServeExit("metrics", b.server.Addr, b.server.Serve(b.listener))
}

func (b *boundMetrics) rollback() error {
	if err := b.listener.Close(); err != nil {
		return fmt.Errorf("rollback close metrics %s: %w", b.server.Addr, err)
	}
	return nil
}

// boundHealth owns one attempt's already-serving health server; done closes when its serve goroutine exits so teardown can await it.
type boundHealth struct {
	server   *http.Server
	listener net.Listener
	done     chan struct{}
}

func (b *boundHealth) serve() {
	defer close(b.done)
	logServeExit("health", b.server.Addr, b.server.Serve(b.listener))
}

// rollback fully retires the early-serving health server: close it, await the serve goroutine, and join any close error.
func (b *boundHealth) rollback() error {
	err := b.server.Close()
	<-b.done
	if err != nil {
		return fmt.Errorf("rollback close health %s: %w", b.server.Addr, err)
	}
	return nil
}

// shutdown drains the health server and awaits its serve goroutine; serverRun.shutdown calls it last so probes get answers for the whole grace period.
func (b *boundHealth) shutdown(ctx context.Context) error {
	err := b.server.Shutdown(ctx)
	<-b.done
	return err
}

// boundListeners is the typed socket ownership of one Start attempt. It
// cannot confuse configured server controls with the sockets rollback owns.
type boundListeners struct {
	http    []*boundHTTP
	http3   []*boundHTTP3
	metrics *boundMetrics
	health  *boundHealth
}

func (b *boundListeners) rollback() error {
	var errs []error
	for _, listener := range b.http {
		errs = append(errs, listener.rollback())
	}
	for _, listener := range b.http3 {
		errs = append(errs, listener.rollback())
	}
	if b.metrics != nil {
		errs = append(errs, b.metrics.rollback())
	}
	if b.health != nil {
		errs = append(errs, b.health.rollback())
	}
	return errors.Join(errs...)
}

// serve launches the content and metrics serve loops; health is absent because serveHealthEarly already launched it before the prerequisite phase.
func (b *boundListeners) serve() {
	for _, listener := range b.http {
		go listener.serve()
	}
	for _, listener := range b.http3 {
		go listener.serve()
	}
	if b.metrics != nil {
		go b.metrics.serve()
	}
}

func (b *boundListeners) shutdown(ctx context.Context, wg *sync.WaitGroup, errs chan<- error) {
	for _, listener := range b.http {
		goShutdown(ctx, wg, errs, listener.server.Shutdown)
	}
	for _, listener := range b.http3 {
		h3 := listener
		goShutdown(ctx, wg, errs, func(ctx context.Context) error {
			return h3.server.shutdown(ctx, h3.conn)
		})
	}
	if b.metrics != nil {
		goShutdown(ctx, wg, errs, b.metrics.server.Shutdown)
	}
}

// count sizes the shutdown errs channel: one slot per drain goroutine, so health — drained separately, last — is excluded.
func (b *boundListeners) count() int {
	n := len(b.http) + len(b.http3)
	if b.metrics != nil {
		n++
	}
	return n
}

// startAttempt is the sole owner of every resource acquired by one Start
// call until commit transfers those typed handles into serverRun.
type startAttempt struct {
	pools     []*runningPool
	acme      []*acmeRun
	docker    *dockerRun
	listeners boundListeners
	finished  bool
}

func (a *startAttempt) rollback() error {
	if a.finished {
		return nil
	}
	a.finished = true
	for _, run := range a.acme {
		run.stop()
	}
	err := a.listeners.rollback()
	a.docker.stop()
	for _, pool := range a.pools {
		pool.shutdown()
	}
	return err
}

// serveHealthEarly binds and serves the health endpoint before the fallible prerequisite phase, ready still false, so supervisors observe live=200/ready=503 throughout startup.
// INVARIANT: it runs only after Start defers attempt.rollback, which closes the server and awaits the serve goroutine, so a failed Start leaves nothing alive and a retry builds a fresh server.
func (a *startAttempt) serveHealthEarly(s *server) error {
	h := s.cfg.Observability.Health
	if !h.Enabled {
		return nil
	}
	hs := s.buildHealthServer(h)
	ln, err := s.bindTCP(context.Background(), hs.Addr)
	if err != nil {
		return fmt.Errorf("health listen %s: %w", hs.Addr, err)
	}
	b := &boundHealth{server: hs, listener: ln, done: make(chan struct{})}
	a.listeners.health = b
	go b.serve()
	return nil
}

func (a *startAttempt) commit() *serverRun {
	if a.finished {
		panic("statute: start attempt already finished")
	}
	a.finished = true
	return &serverRun{
		pools:     a.pools,
		acme:      a.acme,
		docker:    a.docker,
		listeners: a.listeners,
	}
}

// serverRun owns exactly the resources transferred by the successful Start.
// Shutdown never rediscovers ownership from configured server fields.
type serverRun struct {
	pools     []*runningPool
	acme      []*acmeRun
	docker    *dockerRun
	listeners boundListeners
	mu        sync.Mutex
}

// Start opens all configured listeners and begins serving. Calling it
// after the server has already started returns an error. Start is
// transactional and two-phase: phase one starts the non-listener
// prerequisites and binds every socket that could fail, serving nothing —
// so a failure anywhere rolls back without a request ever having been
// accepted — and only once every fallible step has succeeded does phase
// two launch the serve loops and commit. A failed Start releases
// everything it acquired, joining any rollback failure into the returned
// error, and may be retried once the underlying problem is fixed.
func (s *server) Start() (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return errors.New("already started")
	}
	var attempt startAttempt
	defer func() {
		if !s.started {
			err = errors.Join(err, attempt.rollback())
		}
	}()
	// Health serves first, rollback-owned, so probes are answered while the fallible prerequisite phase runs.
	if err := attempt.serveHealthEarly(s); err != nil {
		return err
	}
	if err := s.startPrerequisites(&attempt); err != nil {
		return err
	}
	if err := s.bindSockets(&attempt); err != nil {
		return err
	}
	// Commit transfers ownership before publishing serve loops. No
	// synchronous failure path remains once traffic can reach a handler.
	s.run = attempt.commit()
	s.started = true
	// Every readiness fact holds at commit; flip before serve so no probe
	// can observe a serving health listener that still reports not ready.
	s.ready.Store(true)
	s.run.listeners.serve()
	warmACMERuns(s.run.acme, true)
	return nil
}

// startPrerequisites starts everything a serving server needs that is
// not a socket: the pool health checkers, the ACME managers with their
// DNS-01 warm-up (which needs no local listener), and the Docker
// provider's initial sync. Each is recorded in rb as it comes up.
func (s *server) startPrerequisites(attempt *startAttempt) error {
	for _, ph := range s.pools {
		attempt.pools = append(attempt.pools, ph.start())
	}
	for src, m := range s.acmeManagers {
		run, err := m.start()
		if err != nil {
			return fmt.Errorf("%s manager (%s): %w", m.name, strings.Join(src.Domains, ", "), err)
		}
		attempt.acme = append(attempt.acme, run)
	}
	warmACMERuns(attempt.acme, false)
	if s.docker != nil {
		run, err := s.docker.start()
		if err != nil {
			return fmt.Errorf("docker: %w", err)
		}
		attempt.docker = run
	}
	return nil
}

// bindSockets binds every remaining socket the configuration calls for —
// each content listener's TCP socket, each HTTP/3 listener's UDP socket,
// the metrics listener — recording them in the attempt; the health socket
// already bound and serves via serveHealthEarly. No serve loop runs here:
// binding everything before serving content is what keeps a failed Start
// invisible. The HTTP/3 socket binds here rather than inside its serve
// goroutine so a bind failure fails Start instead of vanishing into a
// discarded error.
func (s *server) bindSockets(attempt *startAttempt) error {
	for _, hs := range s.listeners {
		// Without a matching resolved listener, serveListener would fall
		// through to plain Serve and expose an HTTPS address in cleartext.
		l, ok := findListener(s.cfg.Listeners, hs.Addr)
		if !ok {
			return fmt.Errorf("listen %s: no resolved listener for this address", hs.Addr)
		}
		ln, err := s.bindTCP(context.Background(), hs.Addr)
		if err != nil {
			return fmt.Errorf("listen %s: %w", hs.Addr, err)
		}
		attempt.listeners.http = append(attempt.listeners.http, &boundHTTP{server: hs, policy: l, listener: ln})
	}
	for _, h3 := range s.http3Servers {
		conn, err := s.bindPacket(context.Background(), h3.addr)
		if err != nil {
			return fmt.Errorf("listen %s/udp: %w", h3.addr, err)
		}
		attempt.listeners.http3 = append(attempt.listeners.http3, &boundHTTP3{server: h3, conn: conn})
	}
	if ms := s.metricsServer; ms != nil {
		ln, err := s.bindTCP(context.Background(), ms.Addr)
		if err != nil {
			return fmt.Errorf("metrics listen %s: %w", ms.Addr, err)
		}
		attempt.listeners.metrics = &boundMetrics{server: ms, listener: ln}
	}
	return nil
}

func (s *server) bindTCP(ctx context.Context, addr string) (net.Listener, error) {
	if s.listenTCP != nil {
		return s.listenTCP(ctx, "tcp", addr)
	}
	return (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
}

func (s *server) bindPacket(ctx context.Context, addr string) (net.PacketConn, error) {
	if s.listenPacket != nil {
		return s.listenPacket(ctx, "udp", addr)
	}
	return (&net.ListenConfig{}).ListenPacket(ctx, "udp", addr)
}

// serveListener runs hs on ln, picking ServeTLS vs Serve based on the
// listener's scheme, and returns whatever ended the loop. Certificate
// material always flows through the cert router installed on
// hs.TLSConfig, so ServeTLS never receives file paths.
func serveListener(hs *http.Server, l *resolved.Listener, ln net.Listener) error {
	if l != nil && l.Scheme == schemeHTTPS && l.Redirect == "" {
		return hs.ServeTLS(ln, "", "")
	}
	return hs.Serve(ln)
}

// isServeShutdown reports whether a serve loop's exit was the deliberate
// kind: its server shut down, or its socket closed under it on purpose.
func isServeShutdown(err error) bool {
	return err == nil || errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed)
}

// logServeExit surfaces a serve loop that died for any reason other than
// shutdown: the server still believes it is serving this address, and a
// silently dead listener is an outage the operator has to hear about.
func logServeExit(kind, addr string, err error) {
	if isServeShutdown(err) {
		return
	}
	log.Printf("statute: %s %s: serve loop exited: %v", kind, addr, err)
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
	// Readiness flips off the moment shutdown begins, before any drain, so
	// probes read not-ready for the whole grace period.
	s.ready.Store(false)
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Shutdown.GracePeriod)
	defer cancel()

	s.mu.Lock()
	run := s.run
	// Repeated under the lock so a Start that was mid-commit cannot leave it true.
	s.ready.Store(false)
	s.mu.Unlock()
	var err error
	if run != nil {
		err = run.shutdown(ctx)
	}

	// Flush pending spans last so traces produced during shutdown still ship.
	if s.tracingShutdown != nil {
		err = errors.Join(err, s.tracingShutdown(ctx))
	}
	return err
}

func (r *serverRun) shutdown(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Stop ACME before listeners so in-flight HTTP-01 work can finish
	// cancellation while its challenge endpoint is still reachable.
	for _, run := range r.acme {
		run.stop()
	}

	var wg sync.WaitGroup
	errs := make(chan error, r.listeners.count())
	r.listeners.shutdown(ctx, &wg, errs)
	wg.Wait()
	close(errs)
	err := joinErrors(errs)

	r.docker.stop()
	// Stop health checkers after listeners drain.
	for _, pool := range r.pools {
		pool.shutdown()
	}
	// Health closes last so probes read live=200/ready=503, never refused, for the whole drain.
	if h := r.listeners.health; h != nil {
		err = errors.Join(err, h.shutdown(ctx))
	}
	return err
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
	// matcher is the compiled host/path IR used directly by the dispatcher.
	matcher docker.Matcher
	// clientPrefixes is the parsed form of the route's ClientIPCIDRs; empty
	// means the route matches any client.
	clientPrefixes []netip.Prefix
}

// findHandler returns the first route matching host, path, and — for routes
// constrained with ClientIPs — the verified client address, in slice order,
// or nil. Host comparison is case-insensitive per RFC 9110; only a
// Traefik-derived matcher additionally folds a trailing FQDN dot. The client
// address resolves lazily, once, and only when a candidate route needs it;
// a client that cannot be parsed never matches a constrained route and
// falls through like any other mismatch.
func findHandler(routes []compiledRoute, host string, req *http.Request) http.Handler {
	var clientAddr netip.Addr
	var clientResolved, clientOK bool
	for _, c := range routes {
		if !routeMatchesRequest(c, host, req.URL.Path) {
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

func routeMatchesRequest(c compiledRoute, host, path string) bool {
	return c.matcher.Match(host, path)
}

// tombstoneHandler is the one fixed refusal every tombstone serves: the
// same 404 a dropped router produced before Config.Fallback existed. It
// does not vary with the rule that produced it, and no other status is
// invented, so a deployment without a fallback sees no change at all.
var tombstoneHandler http.Handler = http.NotFoundHandler()

// fallbackHandler returns the router's terminal stage: the configured
// fallback handler, or net/http's 404 when none is configured.
func (s *server) fallbackHandler() http.Handler {
	if s.cfg.Fallback != nil {
		return s.cfg.Fallback
	}
	return http.NotFoundHandler()
}

// buildRouter returns an http.Handler that dispatches to the matching
// static route in declaration order, then to the docker provider's dynamic
// routes when one is configured, then to that generation's tombstones, then
// to the fallback handler.
//
// INVARIANT: the tombstone tier sits between discovered routes and the
// fallback. A Docker registration whose routes were discarded must not
// reach operator code that no longer knows it asked for a policy statute
// could not supply. Envelopes cover every request such a router matched.
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
		case r.Handler != nil:
			base = r.Handler
		}
		h := wrapMiddleware(r.Middleware, base)
		static = append(static, compiledRoute{
			route:          r,
			handler:        h,
			matcher:        docker.CompileNative(r.Host, r.Pattern),
			clientPrefixes: mustParsePrefixes(r.ClientIPCIDRs),
		})
	}

	fallback := s.fallbackHandler()

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
			if h := findHandler(t.tombstones, host, req); h != nil {
				h.ServeHTTP(w, req)
				return
			}
		}
		fallback.ServeHTTP(w, req)
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

// poolHandler is the reusable configured runtime for an upstream pool. A
// runningPool owns each live health-check generation separately.
type poolHandler struct {
	pool      *resolved.Pool
	primary   []*backendState
	backup    []*backendState
	transport *http.Transport
	picker    picker
	hc        *healthChecker
	// passive is the live passive-health generation; nil until a start
	// with passive health enabled. now is the windows' aging clock.
	passive atomic.Pointer[passiveRun]
	now     func() time.Time
}

// runningPool owns one live generation of a pool's background work while the
// configured handler and transport remain reusable across failed Start attempts.
type runningPool struct {
	handler *poolHandler
	health  *healthRun
	passive *passiveRun
	live    atomic.Bool
	stop    sync.Once
}

func (ph *poolHandler) start() *runningPool {
	r := &runningPool{handler: ph, health: ph.hc.start(), passive: ph.startPassive()}
	r.live.Store(true)
	return r
}

func (r *runningPool) shutdown() {
	if r == nil {
		return
	}
	r.stop.Do(func() {
		r.live.Store(false)
		r.health.stop()
		r.passive.stop()
		r.handler.transport.CloseIdleConnections()
	})
}

func (r *runningPool) isLive() bool {
	return r != nil && r.live.Load()
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
		now:       time.Now,
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
		// record is nil-safe: an absent or stopped pinned generation
		// makes it a no-op.
		bs.rp = newBackendProxy(target, transport, p, func(r *http.Request) {
			passiveRunFromContext(r.Context()).record(bs)
		})
		if b.Backup {
			ph.backup = append(ph.backup, bs)
		} else {
			ph.primary = append(ph.primary, bs)
		}
	}

	all := append(append([]*backendState{}, ph.primary...), ph.backup...)
	// INVARIANT: probes ride the proxy transport so TLS verification
	// cannot drift between probe and proxy traffic, and the probe Host
	// has one precedence rule: HealthCheck.Host, else an explicit pool
	// Host, else the backend's own host. UpstreamHost governs proxied
	// requests regardless.
	probeHost := p.HealthCheck.Host
	if probeHost == "" && p.UpstreamHost == resolved.HostExplicit {
		probeHost = p.HostValue
	}
	// hc.start is owned by the caller (server.Start, or the docker
	// provider for label-derived pools), not construction.
	ph.hc = newHealthChecker(p.HealthCheck, all, transport, probeHost)
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

// newBackendProxy builds one backend's reverse proxy. recordFailure is the
// passive-health hook, invoked once per failed attempt: on the transport
// error path through ErrorHandler, or on a 5xx response through
// ModifyResponse — the two paths are mutually exclusive per attempt.
func newBackendProxy(target *url.URL, transport *http.Transport, p *resolved.Pool, recordFailure func(*http.Request)) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		FlushInterval: p.Transport.FlushInterval,
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
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode >= http.StatusInternalServerError {
				recordFailure(resp.Request)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// SAFETY: a client abort lands here too, and recording it
			// would hand unauthenticated clients a pool-wide demotion
			// lever; deadlines and genuine transport failures still
			// count.
			if !errors.Is(r.Context().Err(), context.Canceled) {
				recordFailure(r)
			}
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

// candidates returns the active backend set: available primaries when any
// are, else available backups, else all primaries (degraded mode — better
// to attempt a probably-failing dial than to flat-out 503). The degraded
// floor deliberately ignores passive demotion too, so a single-backend pool
// whose only backend is passively demoted keeps receiving traffic.
func (ph *poolHandler) candidates() []*backendState {
	run := ph.passive.Load()
	if c := available(ph.primary, run); len(c) > 0 {
		return c
	}
	if c := available(ph.backup, run); len(c) > 0 {
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
	// Pin the generation current at attempt start: each Retry re-entry
	// records into its own generation, even across a swap.
	if run := ph.passive.Load(); run != nil {
		r = r.WithContext(withPassiveRun(r.Context(), run))
	}
	bs.rp.ServeHTTP(w, r)
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
