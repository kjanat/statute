package statute

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"statute.kjanat.dev/internal/docker"
	"statute.kjanat.dev/resolved"
)

// dynamicTable is one immutable generation of label-derived routing state.
// The server swaps whole generations atomically. Workload routes additionally
// validate their handler-carried policy revision after a readiness wait.
type dynamicTable struct {
	routes []compiledRoute
	// quarantines retain container provenance between valid routes and tombstones.
	quarantines []compiledRoute
	// tombstones are the refusal envelopes of the registrations this
	// generation discarded; see compileTombstones.
	tombstones []compiledRoute
	pools      map[string]*runningPool
	// workloadBindings and workloadRevisions keep container incarnation and
	// handler-carried routing policy as separate compatibility dimensions.
	workloadBindings  map[string]workloadBindingKey
	workloadRevisions map[string]workloadRoutingRevision
	workloadMutations map[string]uint64
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
	// syncMu serializes reconciles requested by the event loop, refreshes,
	// and coalesced activation demand.
	syncMu sync.Mutex
	// warned dedupes label warnings across reconciles so a misconfigured
	// container logs once per provider lifetime. Guarded by syncMu.
	warned map[string]bool
	// refusal is the previous generation's refusal announcement, or "" when
	// it refused nothing; see announceRefusal.
	refusal string
	// generationChanged closes after each successful dynamic-table
	// publication. Activations use it to await a coalesced reconcile.
	generationMu      sync.Mutex
	generationChanged chan struct{}
	reconciling       bool
	reconcileDemanded bool
	mutationVersions  map[string]uint64

	// workloadEntries is the on-demand lifecycle registry, keyed by
	// discovered-service identity; entries outlive generation swaps.
	workloadMu      sync.Mutex
	workloadEntries map[string]*workload
	// retiredMutations keep predecessor workloads alive while their issued
	// stops settle independently from a successor using the same service key.
	retiredMutations []*workload
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
	// trackMu guards stopping, so no goroutine joins wg once stop began.
	trackMu  sync.Mutex
	stopping bool
	idleMu   sync.RWMutex
	idleOff  atomic.Bool
}

// track runs f on the run's context under its WaitGroup, so stop awaits it.
// It reports false once the run is stopping.
func (r *dockerRun) track(f func(context.Context)) bool {
	r.trackMu.Lock()
	if r.stopping || r.ctx.Err() != nil {
		r.trackMu.Unlock()
		return false
	}
	r.wg.Add(1)
	r.trackMu.Unlock()
	go func() {
		defer r.wg.Done()
		f(r.ctx)
	}()
	return true
}

// trackCancelable is track with a child context the workload registry may
// cancel when a container binding is superseded.
func (r *dockerRun) trackCancelable(f func(context.Context)) (context.CancelFunc, bool) {
	r.trackMu.Lock()
	if r.stopping || r.ctx.Err() != nil {
		r.trackMu.Unlock()
		return nil, false
	}
	ctx, cancel := context.WithCancel(r.ctx)
	r.wg.Add(1)
	r.trackMu.Unlock()
	go func() {
		defer r.wg.Done()
		defer cancel()
		f(ctx)
	}()
	return cancel, true
}

const (
	// dockerDebounce coalesces bursts of container events (compose up starts
	// many containers at once) into a single reconcile.
	dockerDebounce = 300 * time.Millisecond
	// A triggered reconcile owns publication until success. This prevents one
	// transient Docker error from stranding lifecycle quarantine indefinitely.
	dockerReconcileRetryBase = 250 * time.Millisecond
	dockerReconcileRetryCap  = 5 * time.Second
)

func newDockerProvider(cfg *resolved.Docker, srv *server) (*dockerProvider, error) {
	client, err := docker.NewClient(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	return &dockerProvider{
		cfg:               cfg,
		client:            client,
		srv:               srv,
		warned:            make(map[string]bool),
		generationChanged: make(chan struct{}),
		mutationVersions:  make(map[string]uint64),
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
		r.stop()
		return nil, err
	}

	if err := p.client.Ping(ctx); err != nil {
		return fail(err)
	}
	if err := p.sync(ctx); err != nil {
		return fail(err)
	}
	p.resumeWorkloadLifecycles()

	r.wg.Go(r.watchEvents)
	r.wg.Go(r.reconcileLoop)
	return r, nil
}

func (r *dockerRun) stop() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() {
		r.trackMu.Lock()
		r.stopping = true
		r.trackMu.Unlock()
		r.quiesceWorkloads()
		r.cancel()
		r.wg.Wait()
		r.provider.stopWorkloadTimers()
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

func (r *dockerRun) quiesceWorkloads() {
	if r == nil {
		return
	}
	p := r.provider
	p.lifecycleMu.Lock()
	current := p.current == r
	p.lifecycleMu.Unlock()
	if !current {
		return
	}
	r.idleMu.Lock()
	r.idleOff.Store(true)
	p.stopWorkloadTimers()
	r.idleMu.Unlock()
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
			r.reconcileUntilPublished()
		case <-tick:
			r.reconcileUntilPublished()
		}
	}
}

func (r *dockerRun) reconcileUntilPublished() {
	p := r.provider
	p.beginReconcile()
	defer func() {
		if p.finishReconcile() && r.ctx.Err() == nil {
			r.trigger()
		}
	}()

	for delay := dockerReconcileRetryBase; r.ctx.Err() == nil; delay = min(delay*2, dockerReconcileRetryCap) {
		if p.syncLogged(r.ctx) {
			return
		}
		select {
		case <-r.ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// beginReconcile binds activation demand to the publication now in flight.
func (p *dockerProvider) beginReconcile() {
	p.generationMu.Lock()
	p.reconciling = true
	p.reconcileDemanded = false
	p.generationMu.Unlock()
}

// finishReconcile reports demand that arrived after the last publication, or
// while a failed reconcile could not satisfy its waiters.
func (p *dockerProvider) finishReconcile() bool {
	p.generationMu.Lock()
	defer p.generationMu.Unlock()
	demanded := p.reconcileDemanded
	p.reconciling = false
	p.reconcileDemanded = false
	return demanded
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

// syncLogged runs a reconcile, logging instead of propagating errors. It
// reports whether a new generation was published.
func (p *dockerProvider) syncLogged(ctx context.Context) bool {
	if err := p.sync(ctx); err != nil {
		log.Printf("statute: docker: sync failed, keeping previous routes: %v", err)
		return false
	}
	return true
}

// sync lists containers, derives services from labels, builds the next
// route-table generation, swaps it in, and retires replaced pool handlers.
// Reconciles are serialized; successful publication wakes activations waiting
// for a started container's backend to materialize.
func (p *dockerProvider) sync(ctx context.Context) error {
	p.syncMu.Lock()
	defer p.syncMu.Unlock()
	for {
		versions := p.currentMutationVersions()
		containers, err := p.client.ListContainers(ctx)
		if err != nil {
			return err
		}
		contributions := p.deriveContributions(containers)
		observed, _ := mergeContributions(contributions, nil)
		quarantine := p.updateWorkloads(observed, containers, p.multiServiceContainers(containers)) //nolint:contextcheck // observations spawn provider-run work
		p.publishContributionWarnings(contributions, quarantine)
		services, tombstones := mergeContributions(contributions, quarantine.matches)
		quarantine.routes = p.quarantineRouteClaims(containers, quarantine)

		prev := p.srv.dynamic.Load()
		// Pool health checkers deliberately outlive this sync call; they derive
		// their own lifetime and stop on generation retirement or shutdown.
		next, retired := p.buildTable(services, tombstones, quarantine.routes, prev) //nolint:contextcheck
		next.workloadMutations = versions
		if !p.publishGeneration(next, versions) {
			shutdownUnpublishedPools(next, prev)
			continue
		}
		for _, pool := range retired {
			pool.shutdown()
		}
		return nil
	}
}

func (p *dockerProvider) currentMutationVersions() map[string]uint64 {
	p.generationMu.Lock()
	defer p.generationMu.Unlock()
	return maps.Clone(p.mutationVersions)
}

func (p *dockerProvider) publishGeneration(next *dynamicTable, mutationVersions map[string]uint64) bool {
	p.generationMu.Lock()
	defer p.generationMu.Unlock()
	if !maps.Equal(p.mutationVersions, mutationVersions) {
		return false
	}
	p.srv.dynamic.Store(next)
	// Demand recorded before this point waits on the edge closed below and is
	// satisfied by this publication. Later demand schedules another reconcile.
	p.reconcileDemanded = false
	if p.generationChanged != nil {
		close(p.generationChanged)
	}
	p.generationChanged = make(chan struct{})
	return true
}

func shutdownUnpublishedPools(next, prev *dynamicTable) {
	for name, pool := range next.pools {
		if prev == nil || prev.pools[name] != pool {
			pool.shutdown()
		}
	}
}

// requestReconcile coalesces activation demand onto the provider run's event
// loop and returns the successful-publication edge callers can await.
func (p *dockerProvider) requestReconcile() <-chan struct{} {
	p.lifecycleMu.Lock()
	r := p.current
	p.lifecycleMu.Unlock()
	if r == nil {
		return nil
	}
	p.generationMu.Lock()
	changed := p.generationChanged
	reconciling := p.reconciling
	if reconciling {
		p.reconcileDemanded = true
	}
	p.generationMu.Unlock()
	if !reconciling {
		r.trigger()
	}
	return changed
}

// scheduleReconcile requests publication without making the caller wait for
// its generation edge. Mutation settlement uses it to retire quarantines.
func (p *dockerProvider) scheduleReconcile() {
	p.lifecycleMu.Lock()
	r := p.current
	p.lifecycleMu.Unlock()
	if r != nil {
		r.trigger()
	}
}

func (p *dockerProvider) currentRun() *dockerRun {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	return p.current
}

type dockerContribution struct {
	container  docker.Container
	services   []docker.Service
	tombstones []docker.Matcher
	warnings   []string
}

// deriveContributions retains immutable container provenance until lifecycle
// quarantine has selected which contributions may enter ordinary merging.
func (p *dockerProvider) deriveContributions(containers []docker.Container) []dockerContribution {
	sort.Slice(containers, func(i, j int) bool { return containers[i].Name < containers[j].Name })

	opts := p.extractOptions()
	var out []dockerContribution
	for _, c := range containers {
		svcs, envelopes, warns := docker.Extract(c, opts)
		// A stopped container participates only when a workload policy
		// names it; see workloadIntended.
		if !c.Running && !p.workloadIntended(c, opts) {
			continue
		}
		out = append(out, dockerContribution{container: c, services: svcs, tombstones: envelopes, warnings: warns})
	}
	return out
}

func (p *dockerProvider) publishContributionWarnings(contributions []dockerContribution, quarantine retiredMutationQuarantine) {
	for _, contribution := range contributions {
		if !quarantine.matches(contribution.container) {
			p.warn(contribution.warnings)
		}
	}
}

// mergeContributions builds the logical service view after optionally
// excluding exact container contributions. Containers are already ordered, so
// the existing first-container-wins pool policy remains stable.
func mergeContributions(contributions []dockerContribution, excluded func(docker.Container) bool) ([]docker.Service, []docker.Matcher) {
	merged := map[string]*docker.Service{}
	var order []string
	var tombs []docker.Matcher
	for _, contribution := range contributions {
		if excluded != nil && excluded(contribution.container) {
			continue
		}
		tombs = append(tombs, contribution.tombstones...)
		for _, svc := range contribution.services {
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

func (p *dockerProvider) quarantineRouteClaims(containers []docker.Container, quarantine retiredMutationQuarantine) []docker.RouteClaim {
	var out []docker.RouteClaim
	opts := p.extractOptions()
	for _, container := range containers {
		if quarantine.matches(container) {
			out = append(out, docker.RouteClaims(container, opts)...)
		}
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
// settings keep base's (first container wins). Routes differing only in
// middleware references stay separate — the references are router-scoped.
// Contributors accumulates so workload policy can see that this service has
// no single activation owner.
func mergeService(base *docker.Service, add docker.Service) {
	base.Extra = append(base.Extra, add.Backend)
	base.Extra = append(base.Extra, add.Extra...)
	base.Contributors += add.Contributors
	base.Running = base.Running || add.Running
	for _, r := range add.Routes {
		if !slices.ContainsFunc(base.Routes, r.Equal) {
			base.Routes = append(base.Routes, r)
		}
	}
}

// extractOptions is the provider's label-extraction configuration.
func (p *dockerProvider) extractOptions() docker.ExtractOptions {
	return docker.ExtractOptions{
		Network:          p.cfg.Network,
		ExposedByDefault: p.cfg.ExposedByDefault,
		TraefikLabels:    p.cfg.TraefikLabels,
	}
}

// workloadIntended reports whether a code-owned workload policy names any
// service the container's labels could register.
func (p *dockerProvider) workloadIntended(c docker.Container, opts docker.ExtractOptions) bool {
	for _, name := range docker.CandidateServices(c, opts) {
		if _, ok := p.cfg.Workloads[name]; ok {
			return true
		}
	}
	return false
}

// multiServiceContainers names every container whose labels could register
// more than one service. Start and stop act on the whole container, so a
// container beneath several services has no single controllable lifecycle
// owner: one service's idle timer could stop it while another service still
// carries traffic the workload never counted.
func (p *dockerProvider) multiServiceContainers(containers []docker.Container) map[string]bool {
	opts := p.extractOptions()
	var out map[string]bool
	for _, c := range containers {
		if len(docker.CandidateServices(c, opts)) > 1 {
			if out == nil {
				out = map[string]bool{}
			}
			out[c.Name] = true
		}
	}
	return out
}

// buildTable turns derived services into the next dynamic generation,
// reusing pool handlers whose resolved config is unchanged. It returns the
// handlers from prev that were replaced or dropped and must be shut down
// after the swap.
func (p *dockerProvider) buildTable(services []docker.Service, tombstones []docker.Matcher, quarantines []docker.RouteClaim, prev *dynamicTable) (*dynamicTable, []*runningPool) {
	next := &dynamicTable{
		pools:             make(map[string]*runningPool, len(services)),
		fingerprints:      make(map[string]string, len(services)),
		workloadBindings:  make(map[string]workloadBindingKey, len(p.cfg.Workloads)),
		workloadRevisions: make(map[string]workloadRoutingRevision, len(p.cfg.Workloads)),
	}
	tombs := slices.Clone(tombstones)
	matchedPolicy := make(map[string]bool, len(p.cfg.PoolPolicy))
	for _, claim := range quarantines {
		if _, ok := p.cfg.PoolPolicy[claim.Service]; ok {
			matchedPolicy[claim.Service] = true
		}
	}
	for i := range services {
		if _, ok := p.cfg.PoolPolicy[services[i].Name]; ok {
			matchedPolicy[services[i].Name] = true
		}
		tombs = append(tombs, p.addService(&services[i], prev, next)...)
	}
	for _, name := range slices.Sorted(maps.Keys(p.cfg.PoolPolicy)) {
		if !matchedPolicy[name] {
			p.warn([]string{fmt.Sprintf("pool policy %q matches no discovered service; policy is not applied", name)})
		}
	}
	sortDynamicRoutes(next.routes)
	next.quarantines = compileQuarantineRoutes(quarantines)
	sortDynamicRoutes(next.quarantines)
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

// addService compiles one ordinary merged service into the next generation.
func (p *dockerProvider) addService(svc *docker.Service, prev, next *dynamicTable) []docker.Matcher {
	pool, warn := servicePool(svc)
	if warn != "" {
		p.warn([]string{warn})
	}
	gated := p.workloadFor(svc.Name)
	var binding workloadBindingKey
	if gated != nil {
		binding = gated.currentBinding()
		next.workloadBindings[svc.Name] = binding
	}
	rp, err := p.resolveServicePool(svc, pool, gated)
	if err != nil {
		p.warn([]string{fmt.Sprintf("service %q: %v, dropping its routes", svc.Name, err)})
		return p.refuse(svc.Name, svc.Routes)
	}

	kept, tombs := p.routeChains(svc)
	if len(kept) == 0 {
		return tombs
	}
	var revision workloadRoutingRevision
	if gated != nil {
		revision = fingerprintWorkloadRoutes(kept)
		next.workloadRevisions[svc.Name] = revision
	}

	running := p.servicePoolHandler(svc.Name, rp, binding, prev, next)
	if running == nil {
		keptMatchers := make([]docker.Matcher, 0, len(kept))
		for _, rc := range kept {
			keptMatchers = append(keptMatchers, rc.m)
		}
		return append(tombs, p.refuse(svc.Name, keptMatchers)...)
	}
	// The gate resolves the pool at proxy time: the generation that
	// queued a waiter cannot carry a dormant container's backend.
	base := http.Handler(running.handler)
	if gated != nil {
		base = &workloadGate{p: p, w: gated, binding: binding, revision: revision}
	}
	p.appendServiceRoutes(svc.Name, rp, kept, base, gated, binding, revision, next)
	return tombs
}

// compileQuarantineRoutes installs lifecycle outcomes from route envelopes
// alone, after valid routes and before ordinary refusal tombstones.
func compileQuarantineRoutes(claims []docker.RouteClaim) []compiledRoute {
	out := make([]compiledRoute, 0, len(claims))
	for _, claim := range claims {
		m := claim.Matcher
		out = append(out, compiledRoute{
			route:   &resolved.Route{Pattern: m.Path, Host: m.Host},
			handler: workloadMutationQuarantine{},
			service: claim.Service,
			matcher: m,
		})
	}
	return out
}

func (p *dockerProvider) appendServiceRoutes(name string, rp *resolved.Pool, chains []routeChain, base http.Handler, gated *workload, binding workloadBindingKey, revision workloadRoutingRevision, next *dynamicTable) {
	for _, rc := range chains {
		handler := wrapMiddleware(rc.mws, base)
		if gated != nil {
			handler = &workloadRevisionGate{
				p: p, service: name, binding: binding, revision: revision, next: handler,
			}
			handler = &workloadRequestScope{p: p, w: gated, next: handler}
		}
		next.routes = append(next.routes, compiledRoute{
			route: &resolved.Route{
				Pattern:    rc.m.Path,
				Host:       rc.m.Host,
				Upstream:   rp,
				Middleware: rc.mws,
			},
			handler: handler,
			service: name,
			matcher: rc.m,
		})
	}
}

// resolveServicePool resolves the derived pool and overlays the code-owned
// policy. A gated dormant service legitimately has no backends: its stopped
// container has no address. The route must still compile so its gate can keep
// answering 503. Mutation quarantine bypasses pool resolution entirely.
func (p *dockerProvider) resolveServicePool(svc *docker.Service, pool Pool, gated *workload) (*resolved.Pool, error) {
	policy, hasPolicy := preparePoolPolicy(&pool, p.cfg.PoolPolicy, svc.Name)
	resolve := resolvePool
	if gated != nil && len(pool.Backends) == 0 {
		resolve = resolveDormantPool
	}
	rp, err := resolve(svc.Name, pool)
	if err != nil {
		return nil, err
	}
	if hasPolicy {
		applyPoolPolicy(rp, policy)
	}
	return rp, nil
}

// routeChain is one route matcher with its resolved middleware chain.
type routeChain struct {
	m   docker.Matcher
	mws []resolved.Middleware
}

type workloadRoutingRevision string

// fingerprintWorkloadRoutes covers the matcher and middleware semantics held
// by compiled handlers while excluding backend materialization.
func fingerprintWorkloadRoutes(chains []routeChain) workloadRoutingRevision {
	type semantics struct {
		Matcher    docker.Matcher
		Middleware []resolved.Middleware
	}
	view := make([]semantics, len(chains))
	for i := range chains {
		view[i] = semantics{Matcher: chains[i].m, Middleware: chains[i].mws}
	}
	b, err := json.Marshal(view)
	if err != nil {
		return workloadRoutingRevision(fmt.Sprintf("%+v", view))
	}
	return workloadRoutingRevision(b)
}

// routeChains resolves each of the service's routes into its middleware
// chain. A route referencing an unregistered middleware fails closed per
// matcher: it joins the refusal envelope while its siblings keep routing.
func (p *dockerProvider) routeChains(svc *docker.Service) ([]routeChain, []docker.Matcher) {
	hints, warns := serviceHints(svc)
	p.warn(warns)
	var kept []routeChain
	var tombs []docker.Matcher
	for _, m := range svc.Routes {
		mws, warn := p.routeMiddleware(svc, m, hints)
		if warn != "" {
			p.warn([]string{warn})
			tombs = append(tombs, p.refuse(svc.Name, []docker.Matcher{m})...)
			continue
		}
		kept = append(kept, routeChain{m: m, mws: mws})
	}
	return kept, tombs
}

// preparePoolPolicy removes discovered values for fields that code owns before
// resolution. Otherwise an invalid label value could reject the service even
// though the authoritative policy would replace it immediately afterwards.
func preparePoolPolicy(pool *Pool, policies map[string]resolved.PoolPolicy, service string) (resolved.PoolPolicy, bool) {
	policy, ok := policies[service]
	if ok {
		pool.HealthCheck = HealthCheck{}
	}
	return policy, ok
}

// applyPoolPolicy overlays the code-owned fields on a discovered pool. The
// backend set, strategy, and routes are deliberately absent from PoolPolicy and
// therefore remain Docker-owned.
func applyPoolPolicy(pool *resolved.Pool, policy resolved.PoolPolicy) {
	pool.HealthCheck = policy.HealthCheck
	pool.PassiveHealthCheck = policy.PassiveHealthCheck
	pool.Transport = policy.Transport
	pool.UpstreamHost = policy.UpstreamHost
	pool.HostValue = policy.HostValue
}

// refuse turns dropped matchers into a refusal envelope and logs it.
// Widening here would shadow the fallback for traffic the service never asked for.
func (p *dockerProvider) refuse(service string, ms []docker.Matcher) []docker.Matcher {
	env := docker.EnvelopeOf(ms)
	if len(env) == 0 {
		return nil
	}
	p.warn([]string{docker.RefusalWarning(fmt.Sprintf("service %q", service), env)})
	return env
}

// compileTombstones turns a generation's refusal envelopes into routes that
// can only refuse. A global envelope leaves one tombstone after absorption.
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
			matcher: m,
		})
	}
	return out
}

// announceRefusal logs what this generation refuses when that differs from
// the previous generation. warn dedupes for the provider's lifetime; this
// path keys on the previous generation so a repaired-then-regressed rule
// is audible again, and so is the clearing edge.
//
// The clearing line names the refusals that stopped. Config.Fallback is
// optional and this tier does not know whether one is configured.
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
	log.Printf("statute: docker: generation: refusals cleared; unmatched requests are no longer blocked by Docker tombstones")
}

// servicePoolHandler returns the pool handler for the named service. Reuse
// preserves health and connections only across the same resolved config and,
// for a gated workload, the same container incarnation. Nil means construction
// failed and its warning has been logged.
func (p *dockerProvider) servicePoolHandler(name string, rp *resolved.Pool, binding workloadBindingKey, prev, next *dynamicTable) *runningPool {
	fp := poolFingerprint(rp)
	pool := next.pools[name]
	if pool == nil {
		sameBinding := prev != nil && prev.workloadBindings[name] == binding
		if prev != nil && sameBinding && prev.fingerprints[name] == fp && prev.pools[name].isLive() {
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
		a, b := routes[i].matcher, routes[j].matcher
		if (a.Host != "") != (b.Host != "") {
			return a.Host != ""
		}
		aExact, aLen, aKind := dynamicPatternSpecificity(a)
		bExact, bLen, bKind := dynamicPatternSpecificity(b)
		if aExact != bExact {
			return aExact
		}
		if aLen != bLen {
			return aLen > bLen
		}
		if aKind != bKind {
			return aKind > bKind
		}
		if a.Host != b.Host {
			return a.Host < b.Host
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return routes[i].service < routes[j].service
	})
}

// dynamicPatternSpecificity returns the precedence dimensions for one
// dynamic route. Exact paths outrank prefixes. Among prefixes, a longer
// constrained byte sequence is narrower; at an equal base, statute's
// segment prefix is narrower than Traefik's byte prefix.
func dynamicPatternSpecificity(m docker.Matcher) (exact bool, prefixLen, kind int) {
	switch m.PathKind {
	case docker.PathByte:
		return false, len(m.Path), 0
	case docker.PathSegment, docker.PathAny:
		prefix, _ := strings.CutSuffix(m.Path, "/*")
		return false, len(prefix), 1
	default:
		return true, len(m.Path), 2
	}
}
