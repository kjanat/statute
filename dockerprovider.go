package statute

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"statute.kjanat.dev/internal/docker"
	"statute.kjanat.dev/resolved"
)

// dynamicTable is one immutable generation of label-derived routing state.
// The server swaps whole generations atomically; requests in flight keep
// the generation they started with.
type dynamicTable struct {
	routes []compiledRoute
	// tombstones are the refusal envelopes of the registrations this
	// generation discarded; see compileTombstones.
	tombstones []compiledRoute
	pools      map[string]*runningPool
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

	lifecycleMu sync.Mutex
	current     *dockerRun
	// warned dedupes label warnings across reconciles so a misconfigured
	// container logs once, not once per event. Touched only by the sync
	// goroutine (and start, which precedes it).
	warned map[string]bool
	// refusal is the previous generation's refusal announcement, or "" when
	// it refused nothing; see announceRefusal.
	refusal string
}

// dockerRun owns one provider generation's watcher, reconcile loop, and
// dynamic table. The provider keeps only reusable client and policy state.
type dockerRun struct {
	provider *dockerProvider
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	kick     chan struct{}
	stopOnce sync.Once
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
		warned: make(map[string]bool),
	}, nil
}

// start verifies the daemon is reachable, performs the initial synchronous
// sync so listeners open with routes in place, and launches the watch
// loops. An unreachable daemon at startup is fatal — that is a
// misconfiguration, and statute fails fast on those.
func (p *dockerProvider) start() (*dockerRun, error) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &dockerRun{provider: p, ctx: ctx, cancel: cancel, kick: make(chan struct{}, 1)}
	p.lifecycleMu.Lock()
	if p.current != nil {
		p.lifecycleMu.Unlock()
		cancel()
		return nil, fmt.Errorf("already started")
	}
	p.current = r
	p.lifecycleMu.Unlock()
	fail := func(err error) (*dockerRun, error) {
		cancel()
		p.lifecycleMu.Lock()
		if p.current == r {
			p.current = nil
		}
		p.lifecycleMu.Unlock()
		return nil, err
	}

	if err := p.client.Ping(ctx); err != nil {
		return fail(err)
	}
	if err := p.sync(ctx); err != nil {
		return fail(err)
	}

	r.wg.Go(r.watchEvents)
	r.wg.Go(r.reconcileLoop)
	return r, nil
}

func (r *dockerRun) stop() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() {
		r.cancel()
		r.wg.Wait()
		p := r.provider
		p.lifecycleMu.Lock()
		if p.current != r {
			p.lifecycleMu.Unlock()
			return
		}
		p.current = nil
		table := p.srv.dynamic.Swap(nil)
		p.lifecycleMu.Unlock()
		if table != nil {
			for _, pool := range table.pools {
				pool.shutdown()
			}
		}
	})
}

// watchEvents follows the container event stream, kicking the sync loop on
// topology changes and resyncing after every reconnect to cover events
// missed while disconnected.
func (r *dockerRun) watchEvents() {
	p, ctx := r.provider, r.ctx
	backoff := time.Second
	for ctx.Err() == nil {
		err := p.client.StreamEvents(ctx, func(ev docker.Event) {
			backoff = time.Second
			if ev.ChangesTopology() {
				r.trigger()
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
		r.trigger()
	}
}

// trigger requests a reconcile without blocking.
func (r *dockerRun) trigger() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// run is the reconcile loop: debounced event kicks plus the optional
// periodic full resync.
func (r *dockerRun) reconcileLoop() {
	p, ctx := r.provider, r.ctx
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
		case <-r.kick:
			r.debounce()
			p.syncLogged(ctx)
		case <-tick:
			p.syncLogged(ctx)
		}
	}
}

// debounce absorbs further kicks for a short window so event bursts
// reconcile once.
func (r *dockerRun) debounce() {
	timer := time.NewTimer(dockerDebounce)
	defer timer.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-r.kick:
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
	services, tombstones := p.deriveServices(containers)

	prev := p.srv.dynamic.Load()
	// Pool health checkers deliberately outlive this sync call; they derive
	// their own lifetime and stop on generation retirement or shutdown.
	next, retired := p.buildTable(services, tombstones, prev) //nolint:contextcheck
	p.srv.dynamic.Store(next)
	for _, pool := range retired {
		pool.shutdown()
	}
	return nil
}

// deriveServices extracts label registrations from every container and
// merges same-named services into one pool. Containers are processed in
// name order so "first container wins" conflict resolution is stable. The
// second result collects the refusal envelopes extraction produced.
func (p *dockerProvider) deriveServices(containers []docker.Container) ([]docker.Service, []docker.Matcher) {
	sort.Slice(containers, func(i, j int) bool { return containers[i].Name < containers[j].Name })

	opts := docker.ExtractOptions{
		Network:          p.cfg.Network,
		ExposedByDefault: p.cfg.ExposedByDefault,
		TraefikLabels:    p.cfg.TraefikLabels,
	}
	merged := map[string]*docker.Service{}
	var order []string
	var tombs []docker.Matcher
	for _, c := range containers {
		svcs, envelopes, warns := docker.Extract(c, opts)
		p.warn(warns)
		tombs = append(tombs, envelopes...)
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
	return out, tombs
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
// settings keep base's (first container wins). Routes differing only in
// middleware references stay separate — the references are router-scoped.
func mergeService(base *docker.Service, add docker.Service) {
	base.Extra = append(base.Extra, add.Backend)
	base.Extra = append(base.Extra, add.Extra...)
	for _, r := range add.Routes {
		if !slices.ContainsFunc(base.Routes, r.Equal) {
			base.Routes = append(base.Routes, r)
		}
	}
}

// buildTable turns derived services into the next dynamic generation,
// reusing pool handlers whose resolved config is unchanged. It returns the
// handlers from prev that were replaced or dropped and must be shut down
// after the swap.
func (p *dockerProvider) buildTable(services []docker.Service, tombstones []docker.Matcher, prev *dynamicTable) (*dynamicTable, []*runningPool) {
	next := &dynamicTable{
		pools:        make(map[string]*runningPool, len(services)),
		fingerprints: make(map[string]string, len(services)),
	}
	tombs := slices.Clone(tombstones)
	for i := range services {
		tombs = append(tombs, p.addService(&services[i], prev, next)...)
	}
	sortDynamicRoutes(next.routes)
	next.tombstones = p.compileTombstones(tombs)

	var retired []*runningPool
	if prev != nil {
		for name, pool := range prev.pools {
			if next.pools[name] != pool {
				retired = append(retired, pool)
			}
		}
	}
	return next, retired
}

// addService resolves one service into a pool handler and its routes,
// appending them to next. Invalid label values skip the service with a
// warning rather than poisoning the whole generation; a route whose
// router references an unregistered middleware name is omitted the same
// way, so the rest of the service keeps routing.
func (p *dockerProvider) addService(svc *docker.Service, prev, next *dynamicTable) []docker.Matcher {
	pool, warn := servicePool(svc)
	if warn != "" {
		p.warn([]string{warn})
	}
	rp, err := resolvePool(svc.Name, pool)
	if err != nil {
		p.warn([]string{fmt.Sprintf("service %q: %v, dropping its routes", svc.Name, err)})
		return p.refuse(svc.Name, svc.Routes)
	}

	hints, warns := serviceHints(svc)
	p.warn(warns)
	type routeChain struct {
		m   docker.Matcher
		mws []resolved.Middleware
	}
	var kept []routeChain
	var tombs []docker.Matcher
	for _, m := range svc.Routes {
		mws, warn := p.routeMiddleware(svc, m, hints)
		if warn != "" {
			p.warn([]string{warn})
			// Fails closed per matcher, so this one refuses on its own
			// while its siblings keep routing.
			tombs = append(tombs, p.refuse(svc.Name, []docker.Matcher{m})...)
			continue
		}
		kept = append(kept, routeChain{m: m, mws: mws})
	}
	if len(kept) == 0 {
		return tombs
	}

	running := p.servicePoolHandler(svc.Name, rp, prev, next)
	if running == nil {
		keptMatchers := make([]docker.Matcher, 0, len(kept))
		for _, rc := range kept {
			keptMatchers = append(keptMatchers, rc.m)
		}
		return append(tombs, p.refuse(svc.Name, keptMatchers)...)
	}
	for _, rc := range kept {
		next.routes = append(next.routes, compiledRoute{
			route: &resolved.Route{
				Pattern:    rc.m.Path,
				Host:       rc.m.Host,
				Upstream:   rp,
				Middleware: rc.mws,
			},
			handler: wrapMiddleware(rc.mws, running.handler),
		})
	}
	return tombs
}

// refuse turns dropped matchers into a refusal envelope and logs it. The
// matchers were parsed successfully, so the envelope is exactly what the
// routes claimed: widening it here would shadow the fallback for traffic
// the service never asked for.
func (p *dockerProvider) refuse(service string, ms []docker.Matcher) []docker.Matcher {
	env := docker.EnvelopeOf(ms)
	if len(env) == 0 {
		return nil
	}
	p.warn([]string{docker.RefusalWarning(fmt.Sprintf("service %q", service), env)})
	return env
}

// compileTombstones turns a generation's refusal envelopes into routes that
// can only refuse. Absorption runs across the whole generation, so a global
// envelope leaves exactly one tombstone and the fallback is disabled once,
// loudly, rather than by a crowd of redundant entries.
func (p *dockerProvider) compileTombstones(ms []docker.Matcher) []compiledRoute {
	env := docker.EnvelopeOf(ms)
	p.announceRefusal(env)
	if len(env) == 0 {
		return nil
	}
	out := make([]compiledRoute, 0, len(env))
	for _, m := range env {
		out = append(out, compiledRoute{
			route:   &resolved.Route{Pattern: m.Path, Host: m.Host},
			handler: tombstoneHandler,
		})
	}
	return out
}

// announceRefusal logs what this generation refuses when that differs from
// what the last one refused. It bypasses warn deliberately: warn dedupes for
// the provider's lifetime, which is right for a label typo an operator fixes
// once and wrong for the fail-closed tier, where a rule that is repaired and
// later regresses would disable the fallback again in silence. Keying on the
// previous generation repeats the announcement without logging once per poll,
// and makes the clearing edge audible: an operator told the fallback was
// switched off is also told when it came back, since the repair is as much an
// operational event as the refusal was.
func (p *dockerProvider) announceRefusal(env []docker.Matcher) {
	msg := ""
	if len(env) > 0 {
		msg = docker.RefusalWarning("generation", env)
	}
	if msg == p.refusal {
		return
	}
	p.refusal = msg
	if msg != "" {
		log.Printf("statute: docker: %s", msg)
		return
	}
	// Reached only from a non-empty refusal: when the previous generation
	// also refused nothing, the dedupe above has already returned.
	log.Printf("statute: docker: generation: refusals cleared, unmatched requests reach the fallback again")
}

// servicePoolHandler returns the pool handler for the named service,
// reusing prev's handler — keeping its health state and connections —
// when the resolved pool config is unchanged. Nil means the handler could
// not be built; the warning has been logged.
func (p *dockerProvider) servicePoolHandler(name string, rp *resolved.Pool, prev, next *dynamicTable) *runningPool {
	fp := poolFingerprint(rp)
	pool := next.pools[name]
	if pool == nil {
		if prev != nil && prev.fingerprints[name] == fp && prev.pools[name].isLive() {
			pool = prev.pools[name]
		} else {
			ph, err := newPoolHandler(rp)
			if err != nil {
				p.warn([]string{fmt.Sprintf("service %q: %v, dropping its routes", name, err)})
				return nil
			}
			pool = ph.start()
		}
		next.pools[name] = pool
		next.fingerprints[name] = fp
	}
	return pool
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

// serviceHints resolves the service-level middleware hints
// (statute.timeout / statute.ratelimit / statute.compress), dropping
// invalid values with a warning.
func serviceHints(svc *docker.Service) ([]resolved.Middleware, []string) {
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

// routeMiddleware assembles one route's chain: provider-wide defaults
// outermost, then the chains this route's router referenced by name in
// label order, then the service-level label hints. A reference to an
// unregistered name fails closed — the returned warning tells the caller
// to omit the route, because serving it without the middleware it asked
// for (an auth policy, say) is the one unacceptable failure mode.
func (p *dockerProvider) routeMiddleware(svc *docker.Service, m docker.Matcher, hints []resolved.Middleware) ([]resolved.Middleware, string) {
	var out []resolved.Middleware
	out = append(out, p.cfg.DefaultMiddleware...)
	for _, name := range m.Middlewares {
		chain, ok := p.cfg.Middleware[name]
		if !ok {
			return nil, fmt.Sprintf("service %q: unknown middleware %q, dropping route %s%s (register it with Docker().Middleware)", svc.Name, name, m.Host, m.Path)
		}
		out = append(out, chain...)
	}
	return append(out, hints...), ""
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
