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
	// workloadStopIssued is an idle or failed-activation cleanup stop in
	// flight. Nothing proxies until the owned mutation settles.
	workloadStopIssued
	// workloadStopUnknown withholds lifecycle authority after stop verification
	// fails, until discovery establishes whether the container is running.
	workloadStopUnknown
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
	case workloadStopUnknown:
		return "stop-unknown"
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
// `stop-issued`. A failed stop whose verification also fails enters
// `stop-unknown` until discovery proves running or stopped. `failed` reaches
// `starting` once its backoff window has passed or an external start cleared it.
var workloadTransitions = map[workloadPhase][]workloadPhase{
	workloadDormant:     {workloadStarting},
	workloadStarting:    {workloadReady, workloadStopIssued, workloadFailed, workloadDormant},
	workloadReady:       {workloadStopPending, workloadDormant},
	workloadStopPending: {workloadReady, workloadStopIssued, workloadDormant},
	workloadStopIssued:  {workloadDormant, workloadReady, workloadStopUnknown, workloadFailed},
	workloadStopUnknown: {workloadDormant, workloadStopIssued, workloadFailed},
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
	// workloadStopRetryCap bounds the delay between convergence attempts for
	// a stop whose response was lost or whose verification was unavailable.
	workloadStopRetryCap = 5 * time.Second
)

// workloadWait is one outcome a request may wait on. A superseded outcome
// belongs to an earlier container binding and must fail that request closed.
type workloadWait struct {
	done       chan struct{}
	superseded bool
	failed     bool
	waiting    int
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
	binding workloadBindingKey
	ref     string
	policy  resolved.Workload
	cancel  context.CancelFunc
}

type workloadStopKind uint8

const (
	workloadIdleStop workloadStopKind = iota
	workloadCleanupStop
)

// workloadStop is one issued Docker mutation, bound to the container that was
// current when the call began. It remains owned while an ambiguous response
// converges through discovery and bounded retries.
type workloadStop struct {
	workloadWait
	kind       workloadStopKind
	binding    workloadBindingKey
	ref        string
	issued     bool
	converging bool
}

type workloadBindingKey uint64

type workloadActivity struct {
	active int
}

// workloadBinding is one container incarnation behind a discovered service.
// key is its immutable lifecycle identity. The Docker call reference is
// metadata that may refine from name to ID without changing that key.
type workloadBinding struct {
	key         workloadBindingKey
	container   string
	containerID string
	activity    workloadActivity
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

func (b *workloadBinding) observe(svc *docker.Service) {
	b.container = svc.Container
	if svc.ContainerID != "" {
		b.containerID = svc.ContainerID
	}
}

// workloadLease is one proxied request's claim on a container binding.
type workloadLease struct {
	binding  workloadBindingKey
	activity *workloadActivity
}

// workload carries the lifecycle state of one on-demand container. The zero
// value of phase is dormant.
type workload struct {
	service string
	policy  resolved.Workload

	mu      sync.Mutex
	phase   workloadPhase
	binding *workloadBinding
	// nextBinding allocates explicit incarnation keys within this service.
	nextBinding workloadBindingKey
	// retired means the current generation no longer grants on-demand
	// authority; no Docker start or stop may be issued any more.
	retired    bool
	activation *workloadActivation
	// stop is non-nil during stop-issued and wakes queued requests when
	// the bound call settles or its container binding is superseded.
	stop *workloadStop
	idle *time.Timer
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

func (w *workload) currentBinding() workloadBindingKey {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.binding == nil {
		return 0
	}
	return w.binding.key
}

func (w *workload) newBindingLocked(svc *docker.Service) {
	w.nextBinding++
	w.binding = &workloadBinding{
		key:         w.nextBinding,
		container:   svc.Container,
		containerID: svc.ContainerID,
	}
}

// bindContainerLocked moves the registry entry to one observation. A new
// container invalidates in-flight work owned by the preceding binding.
func (w *workload) bindContainerLocked(svc *docker.Service) bool {
	if w.binding == nil {
		w.newBindingLocked(svc)
		return false
	}
	if !w.sameContainerLocked(svc) {
		w.supersedeBindingLocked()
		w.newBindingLocked(svc)
		return true
	}
	w.binding.observe(svc)
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
	case workloadStopIssued, workloadStopUnknown:
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
	w.binding.activity.active++
	if w.idle != nil {
		w.idle.Stop()
		w.idle = nil
	}
	return workloadLease{binding: w.binding.key, activity: &w.binding.activity}
}

// end records a finished request for one container binding; the last current
// request arms the idle timer. A stale completion cannot mutate its successor.
func (w *workload) end(p *dockerProvider, lease workloadLease) {
	w.mu.Lock()
	defer w.mu.Unlock()
	lease.activity.active--
	if w.binding == nil || w.binding.key != lease.binding {
		return
	}
	w.armIdleLocked(p)
}

// armIdleLocked starts the idle countdown when the workload is ready and no
// request is in flight.
func (w *workload) armIdleLocked(p *dockerProvider) {
	if w.phase != workloadReady || w.binding.activity.active > 0 || w.retired {
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
func (w *workload) ensureReady(ctx context.Context, p *dockerProvider, expectedBinding workloadBindingKey) (workloadLease, error) {
	for {
		lease, wait, err := w.serveState(p, expectedBinding) //nolint:contextcheck // request context must not own activation
		if err != nil || lease.binding != 0 {
			return lease, err
		}
		select {
		case <-wait.done:
			w.finishWaiting(wait)
			if wait.superseded || wait.failed {
				return workloadLease{}, workloadUnavailable{}
			}
		case <-ctx.Done():
			w.finishWaiting(wait)
			return workloadLease{}, workloadUnavailable{}
		}
	}
}

// finishWaiting releases this request's claim on one exact outcome.
func (w *workload) finishWaiting(wait *workloadWait) {
	w.mu.Lock()
	defer w.mu.Unlock()
	wait.waiting--
}

// serveState reads the phase under the lock and decides what this request
// does next: serve now, wait on the returned channel (an activation outcome
// or a stop confirmation) and re-evaluate, or fail with err. A ready return
// has already registered the request in the active count under the same
// lock that read the phase, closing the gap an idle stop could enter.
func (w *workload) serveState(p *dockerProvider, expectedBinding workloadBindingKey) (lease workloadLease, wait *workloadWait, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.binding == nil || w.binding.key != expectedBinding {
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
		return w.waitForActivationLocked()
	case workloadStopIssued:
		return w.waitForStopLocked()
	case workloadStopUnknown:
		return workloadLease{}, nil, workloadUnavailable{}
	default:
		return workloadLease{}, nil, workloadUnavailable{}
	}
}

func (w *workload) waitForActivationLocked() (workloadLease, *workloadWait, error) {
	if w.activation == nil {
		return workloadLease{}, nil, workloadUnavailable{}
	}
	w.activation.waiting++
	return workloadLease{}, &w.activation.workloadWait, nil
}

func (w *workload) waitForStopLocked() (workloadLease, *workloadWait, error) {
	if w.stop == nil {
		return workloadLease{}, nil, workloadUnavailable{}
	}
	w.stop.waiting++
	return workloadLease{}, &w.stop.workloadWait, nil
}

// beginAndWait starts a single-flight activation and hands its outcome
// channel to the caller. w.mu must be held.
func (w *workload) beginAndWait(p *dockerProvider) (workloadLease, *workloadWait, error) {
	act, err := p.beginActivationLocked(w, false)
	if err != nil {
		return workloadLease{}, nil, err
	}
	act.waiting++
	return workloadLease{}, &act.workloadWait, nil
}

// workloadGate holds requests for an on-demand service until its workload is
// ready, then serves them through the pool of the generation current at that
// moment. A dormant container has no address until it runs, so the pool is
// resolved at proxy time. The routing revision must still match the handler
// that queued the request.
type workloadGate struct {
	p        *dockerProvider
	w        *workload
	binding  workloadBindingKey
	revision workloadRoutingRevision
}

// workloadRevisionGate refuses handlers from a superseded routing policy
// before their middleware chain can run.
type workloadRevisionGate struct {
	p        *dockerProvider
	service  string
	binding  workloadBindingKey
	revision workloadRoutingRevision
	next     http.Handler
}

func (g *workloadRevisionGate) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	if !g.p.currentWorkloadRoute(g.service, g.binding, g.revision) {
		http.Error(rw, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	g.next.ServeHTTP(rw, r)
}

func (g *workloadGate) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	if !g.p.currentWorkloadRoute(g.w.service, g.binding, g.revision) {
		http.Error(rw, "service unavailable", http.StatusServiceUnavailable)
		return
	}
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
	pool := g.p.currentRoutePool(g.w.service, lease.binding, g.revision)
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
		binding: w.binding.key,
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
	if err := p.startActivation(ctx, act); err != nil {
		return err
	}
	return p.awaitReadiness(ctx, w, act)
}

func (p *dockerProvider) startActivation(ctx context.Context, act *workloadActivation) error {
	if act.observe {
		return nil
	}
	sctx, cancel := context.WithTimeout(ctx, act.policy.StartTimeout)
	defer cancel()
	return p.client.StartContainer(sctx, act.ref)
}

func (p *dockerProvider) awaitReadiness(ctx context.Context, w *workload, act *workloadActivation) error {
	rctx, cancel := context.WithTimeout(ctx, act.policy.ReadyTimeout)
	defer cancel()
	for {
		var generationChanged <-chan struct{}
		insp, err := p.client.InspectContainer(rctx, act.ref)
		if err == nil {
			if !insp.Running {
				return errWorkloadStopped
			}
			var ready bool
			ready, generationChanged = p.probeReady(rctx, w, act, insp)
			if ready {
				return nil
			}
		}
		select {
		case <-rctx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("readiness not established within %s", act.policy.ReadyTimeout)
		case <-generationChanged:
		case <-time.After(workloadProbeInterval):
		}
	}
}

// probeReady evaluates one readiness probe. In every mode the discovered
// backend address must have materialized in the current generation first.
// A missing address requests one coalesced reconcile and returns its
// publication edge so the activation can await provider progress.
func (p *dockerProvider) probeReady(ctx context.Context, w *workload, act *workloadActivation, insp docker.InspectState) (bool, <-chan struct{}) {
	addr := p.currentBackendAddr(w.service, act.binding)
	if addr == "" {
		return false, p.requestReconcile()
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
		return insp.Health == "healthy", nil
	case resolved.ReadinessTCP:
		hostport := strings.TrimPrefix(addr, "https://")
		dialer := net.Dialer{Timeout: workloadProbeTimeout}
		conn, err := dialer.DialContext(ctx, "tcp", hostport)
		if err != nil {
			return false, nil
		}
		_ = conn.Close()
		return true, nil
	case resolved.ReadinessHTTP:
		return p.probeHTTP(ctx, w, act), nil
	default:
		return false, nil
	}
}

// probeHTTP issues one readiness GET over the pool's transport, keeping
// probe traffic on the same TLS policy as proxy traffic.
func (p *dockerProvider) probeHTTP(ctx context.Context, w *workload, act *workloadActivation) bool {
	pool := p.currentPool(w.service, act.binding)
	if pool == nil || len(pool.handler.primary) == 0 {
		return false
	}
	target, err := backendURL(pool.handler.primary[0].backend)
	if err != nil {
		return false
	}
	target.Path = act.policy.Readiness.Path
	pctx, cancel := context.WithTimeout(ctx, workloadProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, target.String(), nil)
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

// finishActivation publishes one activation outcome to every waiter. A
// failed start-owned attempt first installs its cleanup stop in workload state,
// then runs that mutation under the same provider-owned goroutine.
func (p *dockerProvider) finishActivation(ctx context.Context, w *workload, act *workloadActivation, err error) {
	out := w.settleActivation(p, act, err)
	elapsed := time.Since(act.started).Round(time.Millisecond)
	service := strconv.Quote(w.service)
	switch {
	case out.superseded:
		log.Printf("statute: docker: workload %s: activation superseded by a newer container binding", service)
	case err == nil:
		log.Printf("statute: docker: workload %s: ready in %s (%d waiting)", service, elapsed, out.waiters)
	case out.abandoned:
		log.Printf("statute: docker: workload %s: activation abandoned by shutdown", service)
	case out.stale:
		log.Printf("statute: docker: workload %s: stale listing corrected, container is stopped", service)
	default:
		errText := strconv.Quote(err.Error())
		log.Printf("statute: docker: workload %s: activation failed after %s (%d waiting): %s", service, elapsed, out.waiters, errText)
		if out.stop != nil {
			p.runOwnedStop(ctx, w, out.stop)
		}
	}
}

// activationOutcome is what settleActivation decided under the lock.
type activationOutcome struct {
	waiters    int
	abandoned  bool
	stale      bool
	superseded bool
	stop       *workloadStop
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
	act.failed = err != nil
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
		if !act.observe && !w.retired {
			w.toLocked(workloadStopIssued)
			out.stop = w.newStopLocked(workloadCleanupStop, act.binding, act.ref)
		} else {
			w.toLocked(workloadFailed)
		}
	}
	w.activation = nil
	close(act.done)
	out.waiters = act.waiting
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
	if w.phase != workloadReady || w.binding.activity.active > 0 || w.retired {
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
	stop := w.newStopLocked(workloadIdleStop, w.binding.key, w.containerRefLocked())
	w.mu.Unlock()
	p.runOwnedStop(ctx, w, stop)
}

func (w *workload) newStopLocked(kind workloadStopKind, binding workloadBindingKey, ref string) *workloadStop {
	stop := &workloadStop{kind: kind, binding: binding, ref: ref, converging: true}
	stop.done = make(chan struct{})
	w.stop = stop
	return stop
}

type workloadStopResult uint8

const (
	workloadStopSucceeded workloadStopResult = iota
	workloadStopRejected
	workloadStopAmbiguous
)

type workloadStopAttempt struct {
	result     workloadStopResult
	stopErr    error
	inspectErr error
}

func (p *dockerProvider) attemptOwnedStop(ctx context.Context, stop *workloadStop) workloadStopAttempt {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workloadStopTimeout)
	err := p.client.StopContainer(sctx, stop.ref)
	cancel()
	if err == nil {
		return workloadStopAttempt{result: workloadStopSucceeded}
	}
	if docker.LifecycleContainerMissing(err) {
		return workloadStopAttempt{result: workloadStopSucceeded, stopErr: err}
	}

	ictx, icancel := context.WithTimeout(context.WithoutCancel(ctx), workloadProbeTimeout)
	insp, inspectErr := p.client.InspectContainer(ictx, stop.ref)
	icancel()
	if inspectErr == nil && !insp.Running {
		return workloadStopAttempt{result: workloadStopSucceeded, stopErr: err}
	}
	if inspectErr == nil && !docker.LifecycleOutcomeAmbiguous(err) {
		return workloadStopAttempt{result: workloadStopRejected, stopErr: err}
	}
	return workloadStopAttempt{result: workloadStopAmbiguous, stopErr: err, inspectErr: inspectErr}
}

type workloadStopApply uint8

const (
	workloadStopObsolete workloadStopApply = iota
	workloadStopUnsettled
	workloadStopSettled
)

func (w *workload) applyStopAttempt(p *dockerProvider, stop *workloadStop, attempt workloadStopAttempt) workloadStopApply {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stop != stop || w.binding == nil || w.binding.key != stop.binding {
		return workloadStopObsolete
	}
	stop.issued = false
	if attempt.result == workloadStopAmbiguous {
		if w.phase == workloadStopIssued {
			w.toLocked(workloadStopUnknown)
		}
		return workloadStopUnsettled
	}
	w.settleStopLocked(p, stop, attempt.result)
	return workloadStopSettled
}

func (w *workload) settleStopLocked(p *dockerProvider, stop *workloadStop, result workloadStopResult) {
	if stop.kind == workloadCleanupStop {
		w.toLocked(workloadFailed)
	} else if result == workloadStopSucceeded {
		w.toLocked(workloadDormant)
	} else {
		w.toLocked(workloadReady)
		w.armIdleLocked(p)
	}
	w.stop = nil
	close(stop.done)
}

func (p *dockerProvider) runOwnedStop(ctx context.Context, w *workload, stop *workloadStop) {
	defer w.finishStopConvergence(stop)
	if !w.beginStopAttempt(stop) {
		return
	}
	result := p.executeOwnedStopAttempt(ctx, w, stop, true)
	if result != workloadStopUnsettled {
		return
	}

	for delay := workloadProbeInterval; ; delay = min(delay*2, workloadStopRetryCap) {
		if !p.waitStopRetry(ctx, w, stop, delay) {
			return
		}
		if !w.beginStopAttempt(stop) {
			return
		}
		if p.executeOwnedStopAttempt(ctx, w, stop, false) != workloadStopUnsettled {
			return
		}
	}
}

func (p *dockerProvider) executeOwnedStopAttempt(ctx context.Context, w *workload, stop *workloadStop, logUnsettled bool) workloadStopApply {
	attempt := p.attemptOwnedStop(ctx, stop)
	result := w.applyStopAttempt(p, stop, attempt)
	if result != workloadStopUnsettled || logUnsettled {
		p.logStopAttempt(w, stop, attempt, result)
	}
	return result
}

func (p *dockerProvider) waitStopRetry(ctx context.Context, w *workload, stop *workloadStop, delay time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(delay):
	}
	changed := p.requestReconcile()
	if changed != nil {
		select {
		case <-ctx.Done():
			return false
		case <-changed:
		case <-time.After(workloadProbeTimeout):
		}
	}
	return w.stopIsCurrent(stop)
}

func (w *workload) beginStopAttempt(stop *workloadStop) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stop != stop || w.binding == nil || w.binding.key != stop.binding {
		return false
	}
	if w.phase == workloadStopUnknown {
		w.toLocked(workloadStopIssued)
	}
	stop.issued = true
	return true
}

func (w *workload) stopIsCurrent(stop *workloadStop) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stop == stop && w.binding != nil && w.binding.key == stop.binding
}

func (w *workload) finishStopConvergence(stop *workloadStop) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stop == stop {
		stop.converging = false
	}
}

func (p *dockerProvider) ensureStopConvergenceLocked(w *workload, stop *workloadStop) {
	if stop == nil || stop.converging {
		return
	}
	stop.converging = true
	if !p.trackRun(func(ctx context.Context) { p.runOwnedStop(ctx, w, stop) }) {
		stop.converging = false
	}
}

func (p *dockerProvider) logStopAttempt(w *workload, stop *workloadStop, attempt workloadStopAttempt, result workloadStopApply) {
	if result == workloadStopObsolete {
		return
	}
	service := strconv.Quote(w.service)
	if result == workloadStopUnsettled {
		inspect := "none"
		if attempt.inspectErr != nil {
			inspect = strconv.Quote(attempt.inspectErr.Error())
		}
		log.Printf("statute: docker: workload %s: stop outcome unknown: stop: %s; inspect: %s", service, strconv.Quote(attempt.stopErr.Error()), inspect)
		return
	}
	if attempt.result == workloadStopRejected {
		log.Printf("statute: docker: workload %s: stop rejected: %s", service, strconv.Quote(attempt.stopErr.Error()))
		return
	}
	if stop.kind == workloadCleanupStop {
		log.Printf("statute: docker: workload %s: failed-activation cleanup settled", service)
		return
	}
	log.Printf("statute: docker: workload %s: stopped after %s idle", service, strconv.Quote(w.policy.IdleAfter.String()))
}

// currentPool returns the named service's pool only when the generation
// carries the caller's explicit container-incarnation key.
func (p *dockerProvider) currentPool(service string, binding workloadBindingKey) *runningPool {
	t := p.srv.dynamic.Load()
	if t == nil || t.workloadBindings[service] != binding {
		return nil
	}
	return t.pools[service]
}

func (p *dockerProvider) currentWorkloadRoute(service string, binding workloadBindingKey, revision workloadRoutingRevision) bool {
	t := p.srv.dynamic.Load()
	return t != nil && t.workloadBindings[service] == binding && t.workloadRevisions[service] == revision
}

func (p *dockerProvider) currentRoutePool(service string, binding workloadBindingKey, revision workloadRoutingRevision) *runningPool {
	t := p.srv.dynamic.Load()
	if t == nil || t.workloadBindings[service] != binding || t.workloadRevisions[service] != revision {
		return nil
	}
	return t.pools[service]
}

// currentBackendAddr returns the first discovered backend address of the
// service's current pool, or "" while the workload is dormant.
func (p *dockerProvider) currentBackendAddr(service string, binding workloadBindingKey) string {
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
	if svc.Running {
		p.observeRunningWorkloadLocked(w)
		return
	}
	p.observeStoppedWorkloadLocked(w)
}

func (p *dockerProvider) observeRunningWorkloadLocked(w *workload) {
	switch w.phase {
	case workloadStopUnknown:
		p.ensureStopConvergenceLocked(w, w.stop)
	case workloadDormant, workloadFailed:
		w.failures = 0
		w.failedUntil = time.Time{}
		if _, err := p.beginActivationLocked(w, true); err == nil {
			log.Printf("statute: docker: workload %q: found running, establishing readiness", w.service)
		}
	case workloadStarting, workloadReady, workloadStopPending, workloadStopIssued:
		// These phases already own the running observation or its in-flight
		// lifecycle mutation.
	}
}

func (p *dockerProvider) observeStoppedWorkloadLocked(w *workload) {
	switch w.phase {
	case workloadReady:
		w.toLocked(workloadDormant)
		w.stopIdleLocked()
		log.Printf("statute: docker: workload %q: container stopped outside statute", w.service)
	case workloadStopPending:
		w.toLocked(workloadDormant)
		w.stopIdleLocked()
	case workloadStopUnknown:
		if w.stop != nil {
			w.settleStopLocked(p, w.stop, workloadStopSucceeded)
		}
	default:
		// dormant and failed already agree; starting and stop-issued
		// resolve through their own in-flight operations.
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
