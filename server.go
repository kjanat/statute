package statute

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/kjanat/statute/resolved"
)

type server struct {
	cfg *resolved.Config

	listeners     []*http.Server // content + redirect listeners
	metricsServer *http.Server

	pools map[string]*poolHandler

	stats *stats

	mu      sync.Mutex
	started bool
}

func newServer(cfg *resolved.Config) (*server, error) {
	s := &server{
		cfg:   cfg,
		pools: make(map[string]*poolHandler, len(cfg.Upstreams)),
		stats: newStats(),
	}

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

	if s.cfg.Observability.AccessLog.Enabled {
		handler = accessLogMiddleware(s.cfg.Observability.AccessLog, handler)
	}
	handler = metricsMiddleware(s.stats, handler)

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
		if l.AutoTLS != nil {
			return nil, errors.New("AutoTLS is wired through the surface API but the runtime needs golang.org/x/crypto/acme/autocert to actually provision certs; configure StaticTLS for now or vendor autocert")
		}
		if l.StaticTLS == nil {
			return nil, errors.New("https listener has no TLS material")
		}
	}
	if l.HTTP3Addr != "" {
		// HTTP/3 requires quic-go; not yet linked. Surfacing as a clear error
		// keeps deployments honest rather than silently degrading.
		return nil, fmt.Errorf("HTTP/3 listener %s: quic-go is not yet linked; remove HTTP3() to start", l.HTTP3Addr)
	}
	return hs, nil
}

func (s *server) buildMetricsServer(m resolved.Metrics) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc(m.Path, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		s.stats.WritePrometheus(w)
	})
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
	for _, hs := range s.listeners {
		hs := hs
		ln, err := net.Listen("tcp", hs.Addr)
		if err != nil {
			return fmt.Errorf("listen %s: %w", hs.Addr, err)
		}
		go func() {
			if l, ok := findListener(s.cfg.Listeners, hs.Addr); ok && l.Scheme == "https" && l.Redirect == "" && l.StaticTLS != nil {
				_ = hs.ServeTLS(ln, l.StaticTLS.CertFile, l.StaticTLS.KeyFile)
				return
			}
			_ = hs.Serve(ln)
		}()
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
	errs := make(chan error, len(s.listeners)+1)

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
	wg.Wait()
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

// poolHandler proxies requests to an upstream pool.
type poolHandler struct {
	pool       *resolved.Pool
	rp         *httputil.ReverseProxy
	round      uint64
	mu         sync.Mutex
	roundIndex int
}

func newPoolHandler(p *resolved.Pool) (*poolHandler, error) {
	ph := &poolHandler{pool: p}
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
	ph.rp = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			b := ph.pickBackend()
			if b == nil {
				return
			}
			target, err := backendURL(b)
			if err != nil {
				return
			}
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host
			// X-Forwarded-* headers
			pr.SetXForwarded()
		},
		Transport: transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		},
	}
	return ph, nil
}

func backendURL(b *resolved.Backend) (*url.URL, error) {
	addr := b.Address
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	return url.Parse(addr)
}

// pickBackend selects a backend per the pool's strategy. MVP: round-robin
// across primary backends; backups are reserved for failover and currently
// unused (failover would require live health-check state, which is stubbed).
func (ph *poolHandler) pickBackend() *resolved.Backend {
	primary := primaryBackends(ph.pool.Backends)
	if len(primary) == 0 {
		return nil
	}
	switch ph.pool.Strategy {
	case resolved.IPHash, resolved.LeastConnections, resolved.RoundRobin, resolved.Weighted:
		// All strategies fall back to round-robin in this MVP.
	}
	ph.mu.Lock()
	idx := ph.roundIndex % len(primary)
	ph.roundIndex++
	ph.mu.Unlock()
	return &primary[idx]
}

func primaryBackends(all []resolved.Backend) []resolved.Backend {
	out := make([]resolved.Backend, 0, len(all))
	for _, b := range all {
		if !b.Backup {
			out = append(out, b)
		}
	}
	return out
}

func (ph *poolHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ph.rp.ServeHTTP(w, r)
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
		// Retry on non-2xx responses requires intercepting the response
		// before headers are committed; that is non-trivial against
		// httputil.ReverseProxy and is left for a real implementation. Pass
		// through for now so the surface API works end-to-end.
		return next
	case resolved.MWCache:
		return next
	case resolved.MWCompress:
		return compressHandler(m.CompressAlgos, next)
	case resolved.MWETag:
		return next
	default:
		return next
	}
}

// keep this import used even if path-based code is removed during edits
var _ = path.Clean
