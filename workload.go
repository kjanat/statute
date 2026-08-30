package statute

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"statute.kjanat.dev/internal/docker"
	"statute.kjanat.dev/resolved"
)

// workloadPhase is the lifecycle of one on-demand Docker workload, the
// per-container contribution beneath a discovered service.
//
// It is deliberately not [backendState]. A backend begins healthy and demotes on
// evidence of failure; a workload begins unavailable and becomes eligible for
// proxying only once readiness is established.
type workloadPhase uint8

const (
	// workloadDormant is a container that exists and is not running.
	workloadDormant workloadPhase = iota
	// workloadStarting is an activation in progress, readiness not yet proven.
	workloadStarting
	// workloadReady is a container whose readiness has been established.
	workloadReady
	// workloadStopPending is an idle stop decided but not yet issued.
	// A Docker stop cannot be cancelled, so this half stays revocable.
	workloadStopPending
	// workloadStopIssued is a stop call in flight. Nothing proxies from
	// here on; an arriving request waits and then activates again.
	workloadStopIssued
	// workloadFailed is an activation that timed out or failed. The
	// workload holds a backoff window before the next attempt.
	workloadFailed
)

func (p workloadPhase) String() string {
	switch p {
	case workloadDormant:
		return "dormant"
	case workloadStarting:
		return "starting"
	case workloadReady:
		return "ready"
	case workloadStopPending:
		return "stop-pending"
	case workloadStopIssued:
		return "stop-issued"
	case workloadFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// serving reports whether a workload in this phase may receive proxied traffic.
func (p workloadPhase) serving() bool { return p == workloadReady }

// workloadTransitions is the complete legal transition set.
//
// Docker may report a stop statute did not initiate, so `starting`, `ready`,
// and `stop-pending` all reach `dormant` without passing through
// `stop-issued`. `stop-issued` reaches `ready` when the stop call failed and
// the container turned out to still be running. `failed` reaches `starting`
// once its backoff window has passed or an external start cleared it.
var workloadTransitions = map[workloadPhase][]workloadPhase{
	workloadDormant:     {workloadStarting},
	workloadStarting:    {workloadReady, workloadFailed, workloadDormant},
	workloadReady:       {workloadStopPending, workloadDormant},
	workloadStopPending: {workloadReady, workloadStopIssued, workloadDormant},
	workloadStopIssued:  {workloadDormant, workloadReady},
	workloadFailed:      {workloadStarting, workloadDormant},
}

const (
	// workloadProbeInterval paces readiness probing during activation.
	workloadProbeInterval = 250 * time.Millisecond
	// workloadProbeTimeout bounds one TCP or HTTP readiness probe.
	workloadProbeTimeout = time.Second
	// workloadStopTimeout bounds an idle stop call. It exceeds the
	// daemon's default 10s kill grace so a clean stop can complete.
	workloadStopTimeout = 30 * time.Second
)

// workloadWait is one outcome a request may wait on. A superseded outcome
// belongs to an earlier container binding and must fail that request closed.
type workloadWait struct {
	done       chan struct{}
	superseded bool
}

// workloadActivation is one single-flight activation attempt. Every request
// waiting for the same dormant workload shares one; the outcome is the
// phase the workload holds once done closes.
type workloadActivation struct {
	workloadWait
	// observe marks an activation without a start call: the container
	// was found running and only readiness has to be established.
	observe bool
	started time.Time
	binding *workloadBinding
	ref     string
	policy  resolved.Workload
	cancel  context.CancelFunc
}

// workloadStop is one issued Docker stop, bound to the container that was
// current when the call began.
type workloadStop struct {
	workloadWait
	ref string
}

// workloadBinding is one container incarnation behind a discovered service.
// Its addressable Docker reference may be refined from name to ID, while the
// binding pointer remains the lifecycle identity carried by requests and
// dynamic generations.
type workloadBinding struct {
	container   string
	containerID string
	active      int
}

func (b *workloadBinding) ref() string {
	if b == nil {
		return ""
	}
	if b.containerID != "" {
		return b.containerID
	}
	return b.container
}

func (b *workloadBinding) sameContainer(svc *docker.Service) bool {
	if b.containerID != "" && svc.ContainerID != "" {
		return b.containerID == svc.ContainerID
	}
	return b.container == svc.Container
}

// workloadLease is one proxied request's claim on a container binding.
type workloadLease struct {
	binding *workloadBinding
}

// workload carries the lifecycle state of one on-demand container. The zero
// value of phase is dormant.
type workload struct {
	service string
	policy  resolved.Workload

	mu      sync.Mutex
	phase   workloadPhase
	binding *workloadBinding
	// retired means the current generation no longer grants on-demand
	// authority; no Docker start or stop may be issued any more.
	retired    bool
	activation *workloadActivation
	// stop is non-nil during stop-issued and wakes queued requests when
	// the bound call settles or its container binding is superseded.
	stop *workloadStop
	// waiting counts requests blocked on an activation or stop outcome.
	waiting int
	idle    *time.Timer
	// failures counts consecutive failed activations; failedUntil is the
	// end of the current backoff window.
	failures    int
	failedUntil time.Time
}

// phaseNow returns the current phase.
func (w *workload) phaseNow() workloadPhase {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.phase
}

// to moves the workload to next and reports whether the transition was legal. An
// illegal transition leaves the phase untouched.
func (w *workload) to(next workloadPhase) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.toLocked(next)
}

// toLocked is to with w.mu already held.
func (w *workload) toLocked(next workloadPhase) bool {
	if !slices.Contains(workloadTransitions[w.phase], next) {
		return false
	}
	w.phase = next

	return true
}

// containerRefLocked is the identifier lifecycle calls use: the container ID
// from the latest observation, or the stable name before one exists.
func (w *workload) containerRefLocked() string {
	return w.binding.ref()
}

func (w *workload) sameContainerLocked(svc *docker.Service) bool {
	return w.binding != nil && w.binding.sameContainer(svc)
}

func (w *workload) currentBinding() *workloadBinding {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.binding
}

// bindContainerLocked moves the registry entry to one observation. A new
// container invalidates in-flight work owned by the preceding binding.
func (w *workload) bindContainerLocked(svc *docker.Service) bool {
	if w.binding == nil {
		w.binding = &workloadBinding{container: svc.Container, containerID: svc.ContainerID}
		return false
	}
	if !w.sameContainerLocked(svc) {
		w.supersedeBindingLocked()
		w.binding = &workloadBinding{container: svc.Container, containerID: svc.ContainerID}
		return true
	}
	w.binding.container = svc.Container
	w.binding.containerID = svc.ContainerID
	return false
}

func (w *workload) supersedeBindingLocked() {
	switch w.phase {
	case workloadStarting:
		act := w.activation
		if act != nil {
			act.superseded = true
			if act.cancel != nil {
				act.cancel()
			}
			w.activation = nil
			close(act.done)
		}
		w.toLocked(workloadDormant)
	case workloadReady, workloadStopPending:
		w.toLocked(workloadDormant)
		w.stopIdleLocked()
	case workloadStopIssued:
		stop := w.stop
		if stop != nil {
			stop.superseded = true
			w.stop = nil
			close(stop.done)
		}
		w.toLocked(workloadDormant)
	case workloadDormant, workloadFailed:
	}
}

// beginLocked records one in-flight request and holds off the idle timer.
// w.mu must be held; serveState calls it in the same critical section that
// read the phase.
func (w *workload) beginLocked() workloadLease {
	w.binding.active++
	if w.idle != nil {
		w.idle.Stop()
		w.idle = nil
	}
	return workloadLease{binding: w.binding}
}

// end records a finished request for one container binding; the last current
// request arms the idle timer. A stale completion cannot mutate its successor.
func (w *workload) end(p *dockerProvider, lease workloadLease) {
	w.mu.Lock()
	defer w.mu.Unlock()
	lease.binding.active--
	if w.binding != lease.binding {
		return
	}
	w.armIdleLocked(p)
}

// armIdleLocked starts the idle countdown when the workload is ready and no
// request is in flight.
func (w *workload) armIdleLocked(p *dockerProvider) {
	if w.phase != workloadReady || w.binding.active > 0 || w.retired {
		return
	}
	if w.idle != nil {
		w.idle.Stop()
	}
	w.idle = time.AfterFunc(w.policy.IdleAfter, func() { p.idleExpire(w) })
}

// stopIdleLocked cancels a pending idle countdown.
func (w *workload) stopIdleLocked() {
	if w.idle != nil {
		w.idle.Stop()
		w.idle = nil
	}
}

// workloadUnavailable is the terminal per-request outcome of a failed or
// impossible activation. The route answers 503; it never falls through to
// Config.Fallback, because operator code that never asked for the workload
// cannot answer for it.
type workloadUnavailable struct {
	// retryAfter is the remaining backoff window, or zero when no bound
	// is known.
	retryAfter time.Duration
}

func (e workloadUnavailable) Error() string {
	if e.retryAfter > 0 {
		return fmt.Sprintf("workload unavailable, retry after %s", e.retryAfter)
	}
	return "workload unavailable"
}

// ensureReady blocks until the workload is ready to serve, activating it if
// needed. ctx is the request's context: its cancellation abandons this
// request's wait and nothing else. The activation itself runs on the provider
// run's context, so the remaining waiters keep the outcome they need.
func (w *workload) ensureReady(ctx context.Context, p *dockerProvider, expectedBinding *workloadBinding) (workloadLease, error) {
	for {
		lease, wait, err := w.serveState(p, expectedBinding) //nolint:contextcheck // request context must not own activation
		if err != nil || lease.binding != nil {
			return lease, err
		}
		w.addWaiting(1)
		select {
		case <-wait.done:
			w.addWaiting(-1)
			if wait.superseded {
				return workloadLease{}, workloadUnavailable{}
			}
		case <-ctx.Done():
			w.addWaiting(-1)
			return workloadLease{}, workloadUnavailable{}
		}
	}
}

// addWaiting adjusts the count of requests blocked on an outcome.
func (w *workload) addWaiting(delta int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.waiting += delta
}

// serveState reads the phase under the lock and decides what this request
// does next: serve now, wait on the returned channel (an activation outcome
// or a stop confirmation) and re-evaluate, or fail with err. A ready return
// has already registered the request in the active count under the same
// lock that read the phase, closing the gap an idle stop could enter.
func (w *workload) serveState(p *dockerProvider, expectedBinding *workloadBinding) (lease workloadLease, wait *workloadWait, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.binding != expectedBinding {
		return workloadLease{}, nil, workloadUnavailable{}
	}
	return w.serveCurrentStateLocked(p)
}

func (w *workload) serveCurrentStateLocked(p *dockerProvider) (lease workloadLease, wait *workloadWait, err error) {
	switch w.phase {
	case workloadReady:
		return w.beginLocked(), nil, nil
	case workloadStopPending:
		// Revoke the pending stop: no Docker call has been issued yet,
		// so the workload simply keeps serving.
		w.toLocked(workloadReady)
		return w.beginLocked(), nil, nil
	case workloadFailed:
		if remaining := time.Until(w.failedUntil); remaining > 0 {
			return workloadLease{}, nil, workloadUnavailable{retryAfter: remaining}
		}
		return w.beginAndWait(p)
	case workloadDormant:
		return w.beginAndWait(p)
	case workloadStarting:
		if w.activation == nil {
			return workloadLease{}, nil, workloadUnavailable{}
		}
		return workloadLease{}, &w.activation.workloadWait, nil
	case workloadStopIssued:
		if w.stop == nil {
			return workloadLease{}, nil, workloadUnavailable{}
		}
		return workloadLease{}, &w.stop.workloadWait, nil
	default:
		return workloadLease{}, nil, workloadUnavailable{}
	}
}

// beginAndWait starts a single-flight activation and hands its outcome
// channel to the caller. w.mu must be held.
func (w *workload) beginAndWait(p *dockerProvider) (workloadLease, *workloadWait, error) {
	act, err := p.beginActivationLocked(w, false)
	if err != nil {
		return workloadLease{}, nil, err
	}
	return workloadLease{}, &act.workloadWait, nil
}

// workloadGate holds requests for an on-demand service until its workload is
// ready, then serves them through the pool of the generation current at that
// moment. The generation that queued a waiter cannot carry the backend: a
// dormant container has no address until it runs, so the pool is resolved at
// proxy time while route identity and middleware stay those of the matched
// route.
type workloadGate struct {
	p *dockerProvider
	w *workload
	// binding is the immutable container identity of the route generation
	// that constructed this gate.
	binding *workloadBinding
}

func (g *workloadGate) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	lease, err := g.w.ensureReady(r.Context(), g.p, g.binding)
	if err != nil {
		var unavailable workloadUnavailable
		if errors.As(err, &unavailable) && unavailable.retryAfter > 0 {
			rw.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(unavailable.retryAfter.Seconds()))))
		}
		http.Error(rw, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	// ensureReady registered this request in the active count.
	defer g.w.end(g.p, lease)
	pool := g.p.currentPool(g.w.service, lease.binding)
	if pool == nil || !pool.isLive() {
		http.Error(rw, "no backends available", http.StatusServiceUnavailable)
		return
	}
	pool.handler.ServeHTTP(rw, r)
}

// beginActivationLocked starts one single-flight activation. w.mu must be
// held; the legal entry phases are dormant and failed. The attempt runs on
// the provider run's context and is tracked by its WaitGroup, so shutdown
// awaits it. When no run is alive the request fails closed.
func (p *dockerProvider) beginActivationLocked(w *workload, observe bool) (*workloadActivation, error) {
	if w.retired {
		return nil, workloadUnavailable{}
	}
	if !w.toLocked(workloadStarting) {
		return nil, workloadUnavailable{}
	}
	act := &workloadActivation{
		observe: observe,
		started: time.Now(),
		binding: w.binding,
		ref:     w.containerRefLocked(),
		policy:  w.policy,
	}
	act.done = make(chan struct{})
	w.activation = act
	cancel, tracked := p.trackRunCancelable(func(ctx context.Context) { p.activate(ctx, w, act) })
	if !tracked {
		w.activation = nil
		w.toLocked(workloadDormant)
		return nil, workloadUnavailable{}
	}
	act.cancel = cancel
	return act, nil
}

// activate runs one activation attempt to completion and publishes its
// outcome. It runs regardless of waiter disconnects: a Docker start cannot
// be cancelled, cancelling would livelock clients whose timeout is shorter
// than the cold start, and an unclaimed ready workload is reclaimed by the
// idle policy.
func (p *dockerProvider) activate(ctx context.Context, w *workload, act *workloadActivation) {
	err := p.runActivation(ctx, w, act)
	p.finishActivation(ctx, w, act, err)
}

// runActivation starts the container unless the activation is observe-only,
// then establishes readiness within the policy's deadline.
func (p *dockerProvider) runActivation(ctx context.Context, w *workload, act *workloadActivation) error {
	if !act.observe {
		sctx, cancel := context.WithTimeout(ctx, act.policy.StartTimeout)
		err := p.client.StartContainer(sctx, act.ref)
		cancel()
		if err != nil {
			return err
		}
	}

	rctx, cancel := context.WithTimeout(ctx, act.policy.ReadyTimeout)
	defer cancel()
	for {
		insp, err := p.client.InspectContainer(rctx, act.ref)
		if err == nil {
			if !insp.Running {
				return errWorkloadStopped
			}
			if p.probeReady(rctx, w, act, insp) {
				return nil
			}
		}
		select {
		case <-rctx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("readiness not established within %s", act.policy.ReadyTimeout)
		case <-time.After(workloadProbeInterval):
		}
	}
}

// probeReady evaluates one readiness probe. In every mode the discovered
// backend address must have materialized in the current generation first:
// readiness includes the pool being able to serve the waiters, and the
// address exists only after a reconcile has observed the started container.
func (p *dockerProvider) probeReady(ctx context.Context, w *workload, act *workloadActivation, insp docker.InspectState) bool {
	addr := p.currentBackendAddr(w.service, act.binding)
	if addr == "" {
		p.syncLogged(ctx)
		if addr = p.currentBackendAddr(w.service, act.binding); addr == "" {
			return false
		}
	}
	mode := act.policy.Readiness.Mode
	if mode == resolved.ReadinessAuto {
		if insp.Health != "" {
			mode = resolved.ReadinessDockerHealth
		} else {
			mode = resolved.ReadinessTCP
		}
	}
	switch mode {
	case resolved.ReadinessDockerHealth:
		return insp.Health == "healthy"
	case resolved.ReadinessTCP:
		hostport := strings.TrimPrefix(addr, "https://")
		dialer := net.Dialer{Timeout: workloadProbeTimeout}
		conn, err := dialer.DialContext(ctx, "tcp", hostport)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	case resolved.ReadinessHTTP:
		return p.probeHTTP(ctx, w, act, addr)
	default:
		return false
	}
}

// probeHTTP issues one readiness GET over the pool's transport, keeping
// probe traffic on the same TLS policy as proxy traffic.
func (p *dockerProvider) probeHTTP(ctx context.Context, w *workload, act *workloadActivation, addr string) bool {
	pool := p.currentPool(w.service, act.binding)
	if pool == nil {
		return false
	}
	target := addr
	if !strings.Contains(target, "://") {
		target = "http://" + target
	}
	pctx, cancel := context.WithTimeout(ctx, workloadProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, target+act.policy.Readiness.Path, nil)
	if err != nil {
		return false
	}
	resp, err := pool.handler.transport.RoundTrip(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 400
}

// errWorkloadStopped reports a container observed not running during
// readiness: an exit after our start, or an observe-only activation racing
// a stop that a stale listing had not seen yet.
var errWorkloadStopped = errors.New("container is not running")

// finishActivation publishes one activation outcome to every waiter. On
// failure the workload enters its backoff window, and a container this
// attempt started is stopped again to reclaim its resources. Two failure
// shapes skip the backoff and return to dormant: a shutdown-cancelled
// attempt, which the next boot adopts cleanly, and an observe-only
// attempt whose container turned out stopped, which corrects a stale
// listing.
func (p *dockerProvider) finishActivation(ctx context.Context, w *workload, act *workloadActivation, err error) {
	out := w.settleActivation(p, act, err)
	elapsed := time.Since(act.started).Round(time.Millisecond)
	switch {
	case out.superseded:
		log.Printf("statute: docker: workload %q: activation superseded by a newer container binding", w.service)
	case err == nil:
		log.Printf("statute: docker: workload %q: ready in %s (%d waiting)", w.service, elapsed, out.waiters)
	case out.abandoned:
		log.Printf("statute: docker: workload %q: activation abandoned by shutdown", w.service)
	case out.stale:
		log.Printf("statute: docker: workload %q: stale listing corrected, container is stopped", w.service)
	default:
		log.Printf("statute: docker: workload %q: activation failed after %s (%d waiting): %v", w.service, elapsed, out.waiters, err)
		if out.cleanupRef != "" {
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workloadStopTimeout)
			defer cancel()
			if serr := p.client.StopContainer(sctx, out.cleanupRef); serr != nil {
				log.Printf("statute: docker: workload %q: cleanup stop failed: %v", w.service, serr)
			}
		}
	}
}

// activationOutcome is what settleActivation decided under the lock.
type activationOutcome struct {
	cleanupRef string
	waiters    int
	abandoned  bool
	stale      bool
	superseded bool
}

// settleActivation applies one activation outcome to the state machine and
// wakes every waiter. w.mu is taken here.
func (w *workload) settleActivation(p *dockerProvider, act *workloadActivation, err error) activationOutcome {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.activation != act {
		return activationOutcome{superseded: true}
	}
	out := activationOutcome{
		abandoned: err != nil && errors.Is(err, context.Canceled),
		stale:     err != nil && act.observe && errors.Is(err, errWorkloadStopped),
	}
	switch {
	case err == nil:
		w.toLocked(workloadReady)
		w.failures = 0
		w.failedUntil = time.Time{}
		w.armIdleLocked(p)
	case out.abandoned || out.stale:
		w.toLocked(workloadDormant)
	default:
		w.failures++
		w.failedUntil = time.Now().Add(workloadBackoff(w.policy, w.failures))
		w.toLocked(workloadFailed)
		if !act.observe && !w.retired {
			out.cleanupRef = act.ref
		}
	}
	w.activation = nil
	close(act.done)
	out.waiters = w.waiting
	return out
}

// workloadBackoff is the exponential backoff after the given number of
// consecutive failures, bounded by the policy's cap.
func workloadBackoff(policy resolved.Workload, failures int) time.Duration {
	backoff := policy.BackoffBase
	for i := 1; i < failures && backoff < policy.BackoffCap; i++ {
		backoff *= 2
	}
	return min(backoff, policy.BackoffCap)
}

// idleExpire fires when the idle window elapsed. The decision and the Docker
// call are split: stop-pending is set here and remains revocable until
// performStop wins the race under the workload's lock.
func (p *dockerProvider) idleExpire(w *workload) {
	w.mu.Lock()
	if w.phase != workloadReady || w.binding.active > 0 || w.retired {
		w.mu.Unlock()
		return
	}
	w.toLocked(workloadStopPending)
	w.mu.Unlock()
	if !p.trackRun(func(ctx context.Context) { p.performStop(ctx, w) }) {
		w.mu.Lock()
		if w.phase == workloadStopPending {
			w.toLocked(workloadReady)
		}
		w.mu.Unlock()
	}
}

// performStop issues the idle stop unless a request revoked it first. The
// stop call survives shutdown cancellation within its own bound, so an
// issued stop always runs to completion.
func (p *dockerProvider) performStop(ctx context.Context, w *workload) {
	w.mu.Lock()
	if w.phase != workloadStopPending {
		w.mu.Unlock()
		return
	}
	if w.retired {
		w.toLocked(workloadReady)
		w.mu.Unlock()
		return
	}
	w.toLocked(workloadStopIssued)
	stop := &workloadStop{ref: w.containerRefLocked()}
	stop.done = make(chan struct{})
	w.stop = stop
	service := w.service
	w.mu.Unlock()

	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workloadStopTimeout)
	err := p.client.StopContainer(sctx, stop.ref)
	cancel()

	running := false
	if err != nil {
		ictx, icancel := context.WithTimeout(context.WithoutCancel(ctx), workloadProbeTimeout)
		insp, ierr := p.client.InspectContainer(ictx, stop.ref)
		icancel()
		if ierr == nil {
			running = insp.Running
		}
	}

	w.mu.Lock()
	if w.stop != stop {
		w.mu.Unlock()
		return
	}
	if running {
		w.toLocked(workloadReady)
		w.armIdleLocked(p)
	} else {
		w.toLocked(workloadDormant)
	}
	w.stop = nil
	close(stop.done)
	w.mu.Unlock()
	if err != nil {
		log.Printf("statute: docker: workload %q: idle stop failed: %v", service, err)
	} else {
		log.Printf("statute: docker: workload %q: stopped after %s idle", service, w.policy.IdleAfter)
	}
}

// currentPool returns the named service's pool only when the generation
// carries the same container binding as the caller.
func (p *dockerProvider) currentPool(service string, binding *workloadBinding) *runningPool {
	t := p.srv.dynamic.Load()
	if t == nil || t.workloadBindings[service] != binding {
		return nil
	}
	return t.pools[service]
}

// currentBackendAddr returns the first discovered backend address of the
// service's current pool, or "" while the workload is dormant.
func (p *dockerProvider) currentBackendAddr(service string, binding *workloadBinding) string {
	pool := p.currentPool(service, binding)
	if pool == nil {
		return ""
	}
	for _, b := range pool.handler.pool.Backends {
		if b.Address != "" {
			return b.Address
		}
	}
	return ""
}

// trackRun hands f to the current provider run's WaitGroup so shutdown
// awaits it. It reports false when no run is alive.
func (p *dockerProvider) trackRun(f func(context.Context)) bool {
	p.lifecycleMu.Lock()
	r := p.current
	p.lifecycleMu.Unlock()
	if r == nil {
		return false
	}
	return r.track(f)
}

func (p *dockerProvider) trackRunCancelable(f func(context.Context)) (context.CancelFunc, bool) {
	p.lifecycleMu.Lock()
	r := p.current
	p.lifecycleMu.Unlock()
	if r == nil {
		return nil, false
	}
	return r.trackCancelable(f)
}

// workloadFor returns the registry entry gating the named service, or nil.
// A retired entry holds no grant and gates nothing; it stays registered
// only to carry its backoff bookkeeping across a recreation.
func (p *dockerProvider) workloadFor(service string) *workload {
	p.workloadMu.Lock()
	defer p.workloadMu.Unlock()
	w := p.workloadEntries[service]
	if w == nil {
		return nil
	}
	w.mu.Lock()
	retired := w.retired
	w.mu.Unlock()
	if retired {
		return nil
	}
	return w
}

// updateWorkloads reconciles the registry with one derived generation: it
// creates entries for newly covered services, feeds each entry the observed
// container state, and retires entries whose grant disappeared. The policy
// applies only to a one-to-one service and container pair; see
// multiServiceContainers.
func (p *dockerProvider) updateWorkloads(services []docker.Service, multiService map[string]bool) {
	p.workloadMu.Lock()
	defer p.workloadMu.Unlock()
	seen := make(map[string]bool, len(p.cfg.Workloads))
	for i := range services {
		svc := &services[i]
		policy, ok := p.cfg.Workloads[svc.Name]
		if !ok {
			continue
		}
		if svc.Contributors > 1 {
			p.warn([]string{fmt.Sprintf("service %q: on-demand workload policy needs one contributing container, found %d; policy not applied", svc.Name, svc.Contributors)})
			continue
		}
		if multiService[svc.Container] {
			p.warn([]string{fmt.Sprintf("service %q: container %q contributes more than one service and a stop acts on all of them; on-demand policy not applied", svc.Name, svc.Container)})
			continue
		}
		seen[svc.Name] = true
		w := p.workloadEntries[svc.Name]
		if w == nil {
			if p.workloadEntries == nil {
				p.workloadEntries = map[string]*workload{}
			}
			w = &workload{service: svc.Name, policy: policy}
			p.workloadEntries[svc.Name] = w
		}
		w.unretire()
		p.observeWorkload(w, svc)
	}
	p.retireMissingLocked(seen)
}

// unretire restores the grant on a retained entry. A retained phase can be
// stale: the container was removed while ready, so running now proves
// nothing. A stale ready or stop-pending resets to dormant, keeping the
// backoff bookkeeping, and the following observation re-proves readiness
// through the observe gate and re-arms the idle timer.
func (w *workload) unretire() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.retired {
		return
	}
	w.retired = false
	if w.phase == workloadReady || w.phase == workloadStopPending {
		w.toLocked(workloadDormant)
		w.stopIdleLocked()
	}
}

// retireMissingLocked retires every entry whose service the generation no
// longer names. A retired entry stays registered with its backoff
// bookkeeping, so a crash-looping container recreated under the same
// service name cannot shed its window; the registry is bounded by the
// compiled policy map. p.workloadMu must be held.
func (p *dockerProvider) retireMissingLocked(seen map[string]bool) {
	for name, w := range p.workloadEntries {
		if seen[name] {
			continue
		}
		w.mu.Lock()
		alreadyRetired := w.retired
		w.retired = true
		w.stopIdleLocked()
		phase := w.phase
		w.mu.Unlock()
		if !alreadyRetired && (phase == workloadReady || phase == workloadStarting) {
			p.warn([]string{fmt.Sprintf("service %q: on-demand grant removed; its container is left as it is", name)})
		}
	}
}

// observeWorkload feeds one discovery observation into the state machine. An
// externally started container enters the same readiness gate as an
// activation, observe-only, and clears any backoff: someone repaired it. An
// externally stopped one reconciles to dormant; in-flight requests fail
// through the normal proxy error path.
func (p *dockerProvider) observeWorkload(w *workload, svc *docker.Service) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.bindContainerLocked(svc) {
		log.Printf("statute: docker: workload %q: container binding replaced", w.service)
	}
	switch {
	case svc.Running:
		if w.phase == workloadDormant || w.phase == workloadFailed {
			w.failures = 0
			w.failedUntil = time.Time{}
			if _, err := p.beginActivationLocked(w, true); err == nil {
				log.Printf("statute: docker: workload %q: found running, establishing readiness", w.service)
			}
		}
	default:
		switch w.phase {
		case workloadReady:
			w.toLocked(workloadDormant)
			w.stopIdleLocked()
			log.Printf("statute: docker: workload %q: container stopped outside statute", w.service)
		case workloadStopPending:
			w.toLocked(workloadDormant)
			w.stopIdleLocked()
		default:
			// dormant and failed already agree; starting and stop-issued
			// resolve through their own in-flight operations.
		}
	}
}

// stopWorkloadTimers cancels every idle countdown; run shutdown calls it so
// no timer fires into a stopped provider.
func (p *dockerProvider) stopWorkloadTimers() {
	p.workloadMu.Lock()
	defer p.workloadMu.Unlock()
	for _, w := range p.workloadEntries {
		w.mu.Lock()
		w.stopIdleLocked()
		w.mu.Unlock()
	}
}
