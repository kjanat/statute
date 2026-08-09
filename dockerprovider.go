package statute

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"sort"
	"strings"
	"time"

	"statute.kjanat.dev/internal/docker"
	"statute.kjanat.dev/resolved"
)

// dynamicTable is one immutable generation of label-derived routing state.
// The server swaps whole generations atomically; requests in flight keep
// the generation they started with.
type dynamicTable struct {
	routes []compiledRoute
	pools  map[string]*poolHandler
	// fingerprints allow the next generation to reuse a pool handler —
	// keeping its health state and connection pool — when its resolved
	// config is unchanged.
	fingerprints map[string]string
}

// dockerProvider watches the Docker daemon and rebuilds the server's
// dynamic route table as labeled containers come and go.
type dockerProvider struct {
	cfg    *resolved.Docker
	client *docker.Client
	srv    *server

	cancel context.CancelFunc
	done   chan struct{}
	// kick coalesces reconcile triggers; buffered so event handlers never block.
	kick chan struct{}
	// warned dedupes label warnings across reconciles so a misconfigured
	// container logs once, not once per event. Touched only by the sync
	// goroutine (and start, which precedes it).
	warned map[string]bool
}

// dockerDebounce coalesces bursts of container events (compose up starts
// many containers at once) into a single reconcile.
const dockerDebounce = 300 * time.Millisecond

func newDockerProvider(cfg *resolved.Docker, srv *server) (*dockerProvider, error) {
	client, err := docker.NewClient(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	return &dockerProvider{
		cfg:    cfg,
		client: client,
		srv:    srv,
		kick:   make(chan struct{}, 1),
		warned: make(map[string]bool),
	}, nil
}

// start verifies the daemon is reachable, performs the initial synchronous
// sync so listeners open with routes in place, and launches the watch
// loops. An unreachable daemon at startup is fatal — that is a
// misconfiguration, and statute fails fast on those.
func (p *dockerProvider) start() error {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})

	if err := p.client.Ping(ctx); err != nil {
		cancel()
		return err
	}
	if err := p.sync(ctx); err != nil {
		cancel()
		return err
	}

	go p.watchEvents(ctx)
	go p.run(ctx)
	return nil
}

func (p *dockerProvider) stop() {
	if p.cancel == nil {
		return
	}
	p.cancel()
	<-p.done
}

// watchEvents follows the container event stream, kicking the sync loop on
// topology changes and resyncing after every reconnect to cover events
// missed while disconnected.
func (p *dockerProvider) watchEvents(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := p.client.StreamEvents(ctx, func(ev docker.Event) {
			backoff = time.Second
			if ev.ChangesTopology() {
				p.trigger()
			}
		})
		if ctx.Err() != nil {
			return
		}
		log.Printf("statute: docker: event stream lost: %v (reconnecting in %s)", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
		p.trigger()
	}
}

// trigger requests a reconcile without blocking.
func (p *dockerProvider) trigger() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

// run is the reconcile loop: debounced event kicks plus the optional
// periodic full resync.
func (p *dockerProvider) run(ctx context.Context) {
	defer close(p.done)

	var tick <-chan time.Time
	if p.cfg.Refresh > 0 {
		t := time.NewTicker(p.cfg.Refresh)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.kick:
			p.debounce(ctx)
			p.syncLogged(ctx)
		case <-tick:
			p.syncLogged(ctx)
		}
	}
}

// debounce absorbs further kicks for a short window so event bursts
// reconcile once.
func (p *dockerProvider) debounce(ctx context.Context) {
	timer := time.NewTimer(dockerDebounce)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.kick:
		case <-timer.C:
			return
		}
	}
}

// syncLogged runs a reconcile, logging instead of propagating errors: a
// failed reconcile keeps the previous generation serving.
func (p *dockerProvider) syncLogged(ctx context.Context) {
	if err := p.sync(ctx); err != nil {
		log.Printf("statute: docker: sync failed, keeping previous routes: %v", err)
	}
}

// sync lists containers, derives services from labels, builds the next
// route-table generation, swaps it in, and retires replaced pool handlers.
func (p *dockerProvider) sync(ctx context.Context) error {
	containers, err := p.client.ListContainers(ctx)
	if err != nil {
		return err
	}
	services := p.deriveServices(containers)

	prev := p.srv.dynamic.Load()
	// Pool health checkers deliberately outlive this sync call; they derive
	// their own lifetime and stop on generation retirement or shutdown.
	next, retired := p.buildTable(services, prev) //nolint:contextcheck
	p.srv.dynamic.Store(next)
	for _, ph := range retired {
		ph.shutdown()
	}
	return nil
}

// deriveServices extracts label registrations from every container and
// merges same-named services into one pool. Containers are processed in
// name order so "first container wins" conflict resolution is stable.
func (p *dockerProvider) deriveServices(containers []docker.Container) []docker.Service {
	sort.Slice(containers, func(i, j int) bool { return containers[i].Name < containers[j].Name })

	opts := docker.ExtractOptions{
		Network:          p.cfg.Network,
		ExposedByDefault: p.cfg.ExposedByDefault,
		TraefikLabels:    p.cfg.TraefikLabels,
	}
	merged := map[string]*docker.Service{}
	var order []string
	for _, c := range containers {
		svcs, warns := docker.Extract(c, opts)
		p.warn(warns)
		for _, svc := range svcs {
			existing, ok := merged[svc.Name]
			if !ok {
				s := svc
				merged[svc.Name] = &s
				order = append(order, svc.Name)
				continue
			}
			mergeService(existing, svc)
		}
	}
	out := make([]docker.Service, 0, len(merged))
	sort.Strings(order)
	for _, name := range order {
		out = append(out, *merged[name])
	}
	return out
}

// warn logs each warning once for the provider's lifetime.
func (p *dockerProvider) warn(warns []string) {
	for _, w := range warns {
		if p.warned[w] {
			continue
		}
		p.warned[w] = true
		log.Printf("statute: docker: %s", w)
	}
}

// mergeService folds a same-named registration from another container into
// base: the backend joins the pool, unseen routes are appended, and pool
// settings keep base's (first container wins).
func mergeService(base *docker.Service, add docker.Service) {
	base.Extra = append(base.Extra, add.Backend)
	base.Extra = append(base.Extra, add.Extra...)
	for _, r := range add.Routes {
		if !slices.Contains(base.Routes, r) {
			base.Routes = append(base.Routes, r)
		}
	}
}

// buildTable turns derived services into the next dynamic generation,
// reusing pool handlers whose resolved config is unchanged. It returns the
// handlers from prev that were replaced or dropped and must be shut down
// after the swap.
func (p *dockerProvider) buildTable(services []docker.Service, prev *dynamicTable) (*dynamicTable, []*poolHandler) {
	next := &dynamicTable{
		pools:        make(map[string]*poolHandler, len(services)),
		fingerprints: make(map[string]string, len(services)),
	}
	for i := range services {
		p.addService(&services[i], prev, next)
	}
	sortDynamicRoutes(next.routes)

	var retired []*poolHandler
	if prev != nil {
		for name, ph := range prev.pools {
			if next.pools[name] != ph {
				retired = append(retired, ph)
			}
		}
	}
	return next, retired
}

// addService resolves one service into a pool handler and its routes,
// appending them to next. Invalid label values skip the service with a
// warning rather than poisoning the whole generation.
func (p *dockerProvider) addService(svc *docker.Service, prev, next *dynamicTable) {
	pool, warn := servicePool(svc)
	if warn != "" {
		p.warn([]string{warn})
	}
	rp, err := resolvePool(svc.Name, pool)
	if err != nil {
		p.warn([]string{fmt.Sprintf("service %q: %v, skipping", svc.Name, err)})
		return
	}
	fp := poolFingerprint(rp)

	ph := next.pools[svc.Name]
	if ph == nil {
		if prev != nil && prev.fingerprints[svc.Name] == fp {
			ph = prev.pools[svc.Name]
		} else {
			var err error
			ph, err = newPoolHandler(rp)
			if err != nil {
				p.warn([]string{fmt.Sprintf("service %q: %v, skipping", svc.Name, err)})
				return
			}
		}
		next.pools[svc.Name] = ph
		next.fingerprints[svc.Name] = fp
	}

	mws, warns := serviceMiddleware(svc)
	p.warn(warns)
	handler := wrapMiddleware(mws, ph)
	for _, m := range svc.Routes {
		next.routes = append(next.routes, compiledRoute{
			route: &resolved.Route{
				Pattern:    m.Path,
				Host:       m.Host,
				Upstream:   rp,
				Middleware: mws,
			},
			handler: handler,
		})
	}
}

// servicePool builds the surface Pool for a derived service so the standard
// resolver applies the same validation and defaulting as static config.
func servicePool(svc *docker.Service) (Pool, string) {
	backends := append([]docker.Backend{svc.Backend}, svc.Extra...)
	var sb []Backend
	seen := map[string]bool{}
	for _, b := range backends {
		if b.Address == "" || seen[b.Address] {
			continue
		}
		seen[b.Address] = true
		sb = append(sb, Backend{Address: b.Address, Weight: b.Weight, Backup: b.Backup})
	}
	sort.Slice(sb, func(i, j int) bool { return sb[i].Address < sb[j].Address })

	strategy, warn := parseStrategy(svc.Name, svc.Strategy)
	return Pool{
		Backends: sb,
		Strategy: strategy,
		HealthCheck: HealthCheck{
			Path:     svc.HealthCheckPath,
			Interval: svc.HealthCheckInterval,
			Timeout:  svc.HealthCheckTimeout,
		},
	}, warn
}

// parseStrategy maps the label value to a Strategy by its canonical
// String() name, defaulting to round-robin with a warning on unknown values.
func parseStrategy(service, s string) (Strategy, string) {
	if s == "" {
		return RoundRobin, ""
	}
	for _, st := range []Strategy{RoundRobin, LeastConnections, IPHash, Weighted} {
		if s == st.String() {
			return st, ""
		}
	}
	return RoundRobin, fmt.Sprintf("service %q: unknown strategy %q, using %s", service, s, RoundRobin)
}

// serviceMiddleware builds the route middleware chain from the service's
// label hints, dropping invalid values with a warning.
func serviceMiddleware(svc *docker.Service) ([]resolved.Middleware, []string) {
	var mws []Middleware
	var warns []string
	if svc.Timeout != "" {
		mws = append(mws, Timeout(svc.Timeout))
	}
	if svc.RateLimit != "" {
		mws = append(mws, RateLimit(svc.RateLimit))
	}
	if svc.Compress != "" {
		algos, warn := parseCompressAlgos(svc.Name, svc.Compress)
		if warn != "" {
			warns = append(warns, warn)
		}
		if len(algos) > 0 {
			mws = append(mws, Compress(algos...))
		}
	}
	out, err := resolveMiddlewares(mws)
	if err != nil {
		return nil, append(warns, fmt.Sprintf("service %q: %v, dropping label middleware", svc.Name, err))
	}
	return out, warns
}

// parseCompressAlgos parses a "gzip,br" style label value.
// "statute.compress=true" means "default algorithms".
func parseCompressAlgos(service, s string) ([]CompressAlgo, string) {
	var algos []CompressAlgo
	for a := range strings.SplitSeq(s, ",") {
		switch strings.TrimSpace(a) {
		case Gzip.String():
			algos = append(algos, Gzip)
		case Brotli.String(), "brotli":
			algos = append(algos, Brotli)
		case "", labelValueTrue:
			algos = append(algos, Gzip, Brotli)
		default:
			return nil, fmt.Sprintf("service %q: unknown compress algorithm %q, dropping compression", service, a)
		}
	}
	return algos, ""
}

// poolFingerprint serializes the resolved pool for change detection.
func poolFingerprint(rp *resolved.Pool) string {
	b, err := json.Marshal(rp)
	if err != nil {
		// resolved.Pool is plain data; Marshal cannot realistically fail.
		return fmt.Sprintf("%+v", rp)
	}
	return string(b)
}

// sortDynamicRoutes orders label-derived routes most-specific first so
// matching is deterministic regardless of container enumeration order:
// host-scoped before host-agnostic, longer path prefixes before shorter,
// then lexicographic as the tiebreak.
func sortDynamicRoutes(routes []compiledRoute) {
	sort.SliceStable(routes, func(i, j int) bool {
		a, b := routes[i].route, routes[j].route
		if (a.Host != "") != (b.Host != "") {
			return a.Host != ""
		}
		al, bl := patternSpecificity(a.Pattern), patternSpecificity(b.Pattern)
		if al != bl {
			return al > bl
		}
		if a.Host != b.Host {
			return a.Host < b.Host
		}
		if a.Pattern != b.Pattern {
			return a.Pattern < b.Pattern
		}
		return a.Upstream.Name < b.Upstream.Name
	})
}

// patternSpecificity ranks patterns: exact matches above prefixes, longer
// prefixes above shorter ones.
func patternSpecificity(pattern string) int {
	if prefix, ok := strings.CutSuffix(pattern, "/*"); ok {
		return len(prefix)
	}
	// Exact patterns outrank any prefix.
	return len(pattern) + 1<<16
}
