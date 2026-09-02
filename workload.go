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
	reserved   int
	lease      workloadLease
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
	uncertain  bool
	converging bool
	persisted  bool
	terminal   bool
	result     workloadStopResult
}

type workloadBindingKey uint64

type workloadActivity struct {
	active int
}

// workloadFailureEvidence records whether discovery proved that the current
// binding stopped after its latest activation failure.
type workloadFailureEvidence uint8

const (
	workloadFailureUnproven workloadFailureEvidence = iota
	workloadFailureStopped
)

// workloadBinding is one container incarnation behind a discovered service.
// key is its immutable lifecycle identity. The Docker call reference is
// metadata that may refine from name to ID without changing that key.
type workloadBinding struct {
	key         workloadBindingKey
	container   string
	containerID string
	activity    workloadActivity
}

// workloadStopOwnership is the exact owner captured around unlocked WAL I/O.
// Pointer, binding key, and immutable ID must all still match before commit.
type workloadStopOwnership struct {
	stop          *workloadStop
	binding       *workloadBinding
	bindingKey    workloadBindingKey
	containerID   string
	containerName string
	service       string
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

func (w *workload) stopOwnershipLocked(stop *workloadStop) (workloadStopOwnership, bool) {
	if stop == nil || w.stop != stop || w.binding == nil || w.binding.key != stop.binding || w.binding.containerID == "" {
		return workloadStopOwnership{}, false
	}
	return workloadStopOwnership{
		stop:          stop,
		binding:       w.binding,
		bindingKey:    w.binding.key,
		containerID:   w.binding.containerID,
		containerName: w.binding.container,
		service:       w.service,
	}, true
}

func (o workloadStopOwnership) currentLocked(w *workload) bool {
	return w.stop == o.stop && w.binding == o.binding && o.binding.key == o.bindingKey &&
		o.stop.binding == o.bindingKey && o.binding.containerID == o.containerID
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

// workloadMutationQuarantine is fixed to one published generation. It keeps
// every route backed by a container with an unsettled retired stop non-serving;
// only a later reconcile after terminal evidence may publish ordinary routes.
type workloadMutationQuarantine struct{}

func (workloadMutationQuarantine) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "workload lifecycle mutation is still settling", http.StatusServiceUnavailable)
}

// workload carries the lifecycle state of one on-demand container. The zero
// value of phase is dormant.
type workload struct {
	service string
	policy  resolved.Workload

	mu      sync.Mutex
	phase   workloadPhase
	binding *workloadBinding
	// hadBinding distinguishes a fresh entry from one whose container was
	// replaced while the binding itself was detached.
	hadBinding bool
	// retired means the current generation no longer grants on-demand
	// authority; no Docker start or stop may be issued any more.
	retired    bool
	activation *workloadActivation
	// stop is non-nil during stop-issued and wakes queued requests when
	// the bound call settles or its container binding is superseded.
	stop      *workloadStop
	idle      *time.Timer
	idleEpoch uint64
	// failureEvidence requires stopped evidence before same-binding running
	// may clear the current activation backoff.
	failures        int
	failedUntil     time.Time
	failureEvidence workloadFailureEvidence
	// observationEpoch rejects Docker snapshots captured before a local
	// lifecycle transition changed how their running state would be interpreted.
	observationEpoch uint64
}

func (p *dockerProvider) invalidateWorkloadObservationsLocked(w *workload) {
	w.observationEpoch++
	if p == nil {
		return
	}
	p.generationMu.Lock()
	p.observationVersion++
	p.generationMu.Unlock()
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

// callRef returns the most specific Docker reference known for one operation
// without letting a stale operation follow a replacement binding.
func (w *workload) callRef(binding workloadBindingKey, fallback string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.binding != nil && w.binding.key == binding {
		if ref := w.binding.ref(); ref != "" {
			return ref
		}
	}
	return fallback
}

func (w *workload) ownsIssuedMutationForOtherContainerLocked(svc *docker.Service) bool {
	if w.stop == nil || (w.phase != workloadStopIssued && w.phase != workloadStopUnknown) {
		return false
	}
	return w.binding != nil && !w.binding.sameContainer(svc)
}

// nextWorkloadBindingLocked allocates one provider-lifetime incarnation key.
// p.workloadMu must be held.
func (p *dockerProvider) nextWorkloadBindingLocked() workloadBindingKey {
	p.nextWorkloadBinding++
	return p.nextWorkloadBinding
}

func (p *dockerProvider) newWorkloadBindingLocked(w *workload, svc *docker.Service) {
	w.binding = &workloadBinding{
		key:         p.nextWorkloadBindingLocked(),
		container:   svc.Container,
		containerID: svc.ContainerID,
	}
	w.hadBinding = true
}

// bindWorkloadContainerLocked moves an entry to one observation. A new
// container invalidates in-flight work owned by the preceding binding.
// p.workloadMu and w.mu must be held.
func (p *dockerProvider) bindWorkloadContainerLocked(w *workload, svc *docker.Service) bool {
	if w.binding == nil {
		p.newWorkloadBindingLocked(w, svc)
		return false
	}
	if !w.sameContainerLocked(svc) {
		w.supersedeBindingLocked()
		p.newWorkloadBindingLocked(w, svc)
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
	if p != nil && !p.idleStopsEnabled() {
		return
	}
	if w.idle != nil {
		w.idle.Stop()
	}
	w.idleEpoch++
	epoch := w.idleEpoch
	binding := w.binding.key
	var run *dockerRun
	if p != nil {
		run = p.currentRun()
	}
	w.idle = time.AfterFunc(w.policy.IdleAfter, func() {
		if p != nil {
			p.idleExpire(w, binding, epoch, run)
		}
	})
}

// stopIdleLocked cancels a pending idle countdown.
func (w *workload) stopIdleLocked() {
	w.idleEpoch++
	if w.idle != nil {
		w.idle.Stop()
		w.idle = nil
	}
	if w.phase == workloadStopPending {
		w.toLocked(workloadReady)
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
			lease, failed := w.finishWaiting(p, wait, true)
			if lease.binding != 0 {
				return lease, nil
			}
			if failed {
				return workloadLease{}, workloadUnavailable{}
			}
		case <-ctx.Done():
			_, _ = w.finishWaiting(p, wait, false)
			return workloadLease{}, workloadUnavailable{}
		}
	}
}

// finishWaiting settles this request's claim on one exact outcome.
func (w *workload) finishWaiting(p *dockerProvider, wait *workloadWait, claim bool) (workloadLease, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	wait.waiting--
	failed := wait.superseded || wait.failed
	if wait.reserved == 0 {
		return workloadLease{}, failed
	}
	wait.reserved--
	lease := wait.lease
	if failed || !claim || w.binding == nil || w.binding.key != lease.binding {
		lease.activity.active--
		if w.binding != nil && w.binding.key == lease.binding {
			w.armIdleLocked(p)
		}
		return workloadLease{}, failed || claim
	}
	return lease, false
}

func (w *workload) reserveWaitersLocked(wait *workloadWait) {
	if wait.waiting == 0 {
		return
	}
	w.binding.activity.active += wait.waiting
	wait.reserved = wait.waiting
	wait.lease = workloadLease{binding: w.binding.key, activity: &w.binding.activity}
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
		return w.beginAndWait(p, w.failureEvidence != workloadFailureStopped)
	case workloadDormant:
		return w.beginAndWait(p, false)
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
func (w *workload) beginAndWait(p *dockerProvider, observe bool) (workloadLease, *workloadWait, error) {
	act, err := p.beginActivationLocked(w, observe)
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

type workloadRevisionGate struct {
	p        *dockerProvider
	service  string
	binding  workloadBindingKey
	revision workloadRoutingRevision
	next     http.Handler
}

type workloadRequestState struct {
	mu       sync.Mutex
	p        *dockerProvider
	w        *workload
	lease    workloadLease
	acquired bool
	active   int
	closed   bool
	released bool
}

type workloadRequestStateKey struct{}

type workloadRequestScope struct {
	p    *dockerProvider
	w    *workload
	next http.Handler
}

func (s *workloadRequestScope) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	state := &workloadRequestState{p: s.p, w: s.w}
	defer state.close()
	ctx := context.WithValue(r.Context(), workloadRequestStateKey{}, state)
	s.next.ServeHTTP(rw, r.WithContext(ctx))
}

func (s *workloadRequestState) beginGate() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.active++
	return true
}

func (s *workloadRequestState) endGate() {
	s.mu.Lock()
	s.active--
	lease, release := s.releaseIfFinishedLocked()
	s.mu.Unlock()
	if release {
		s.w.end(s.p, lease)
	}
}

func (s *workloadRequestState) close() {
	s.mu.Lock()
	s.closed = true
	lease, release := s.releaseIfFinishedLocked()
	s.mu.Unlock()
	if release {
		s.w.end(s.p, lease)
	}
}

func (s *workloadRequestState) releaseIfFinishedLocked() (workloadLease, bool) {
	if !s.closed || s.active != 0 || !s.acquired || s.released {
		return workloadLease{}, false
	}
	s.released = true
	return s.lease, true
}

func (g *workloadRevisionGate) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	if !g.p.currentWorkloadRoute(g.service, g.binding, g.revision) {
		http.Error(rw, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	g.next.ServeHTTP(rw, r)
}

func (g *workloadGate) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	state, scoped := r.Context().Value(workloadRequestStateKey{}).(*workloadRequestState)
	if scoped {
		if !state.beginGate() {
			http.Error(rw, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		defer state.endGate()
	}
	if !g.p.currentWorkloadRoute(g.w.service, g.binding, g.revision) {
		http.Error(rw, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	lease, release, err := g.requestLease(r)
	if err != nil {
		var unavailable workloadUnavailable
		if errors.As(err, &unavailable) && unavailable.retryAfter > 0 {
			rw.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(unavailable.retryAfter.Seconds()))))
		}
		http.Error(rw, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	if release {
		defer g.w.end(g.p, lease)
	}
	pool := g.p.currentRoutePool(g.w.service, lease.binding, g.revision)
	if pool == nil || !pool.isLive() {
		http.Error(rw, "no backends available", http.StatusServiceUnavailable)
		return
	}
	pool.handler.ServeHTTP(rw, r)
}

func (g *workloadGate) requestLease(r *http.Request) (workloadLease, bool, error) {
	state, scoped := r.Context().Value(workloadRequestStateKey{}).(*workloadRequestState)
	if scoped {
		state.mu.Lock()
		defer state.mu.Unlock()
		if state.acquired {
			return state.lease, false, nil
		}
	}
	lease, err := g.w.ensureReady(r.Context(), g.p, g.binding)
	if err != nil {
		return workloadLease{}, false, err
	}
	if scoped {
		state.lease = lease
		state.acquired = true
		return lease, false, nil
	}
	return lease, true, nil
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
	if err := p.startActivation(ctx, w, act); err != nil {
		return err
	}
	return p.awaitReadiness(ctx, w, act)
}

func (p *dockerProvider) startActivation(ctx context.Context, w *workload, act *workloadActivation) error {
	if act.observe {
		return nil
	}
	sctx, cancel := context.WithTimeout(ctx, act.policy.StartTimeout)
	defer cancel()
	return p.client.StartContainer(sctx, w.callRef(act.binding, act.ref))
}

func (p *dockerProvider) awaitReadiness(ctx context.Context, w *workload, act *workloadActivation) error {
	rctx, cancel := context.WithTimeout(ctx, act.policy.ReadyTimeout)
	defer cancel()
	for {
		var generationChanged <-chan struct{}
		insp, err := p.client.InspectContainer(rctx, w.callRef(act.binding, act.ref))
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
		if !waitReadinessProbe(rctx, generationChanged) {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("readiness not established within %s", act.policy.ReadyTimeout)
		}
	}
}

func waitReadinessProbe(ctx context.Context, generationChanged <-chan struct{}) bool {
	select {
	case <-ctx.Done():
		return false
	case <-generationChanged:
		select {
		case <-ctx.Done():
			return false
		case <-time.After(workloadProbeInterval):
			return true
		}
	case <-time.After(workloadProbeInterval):
		return true
	}
}

// probeReady evaluates one readiness probe. In every mode the discovered
// backend address must have materialised in the current generation first.
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
	if pool == nil {
		return false
	}
	backends := pool.handler.primary
	if len(backends) == 0 {
		backends = pool.handler.backup
	}
	if len(backends) == 0 {
		return false
	}
	target, err := backendURL(backends[0].backend)
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
	if pool.handler.pool.UpstreamHost == resolved.HostExplicit {
		req.Host = pool.handler.pool.HostValue
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
	p.invalidateWorkloadObservationsLocked(w)
	out := activationOutcome{
		abandoned: err != nil && errors.Is(err, context.Canceled),
		stale:     err != nil && act.observe && errors.Is(err, errWorkloadStopped),
	}
	act.failed = err != nil
	switch {
	case err == nil:
		w.toLocked(workloadReady)
		w.clearFailureLocked()
		w.reserveWaitersLocked(&act.workloadWait)
		w.armIdleLocked(p)
	case out.abandoned || out.stale:
		w.toLocked(workloadDormant)
	default:
		w.failures++
		w.failedUntil = time.Now().Add(workloadBackoff(w.policy, w.failures))
		w.failureEvidence = workloadFailureUnproven
		if !act.observe && !w.retired {
			w.toLocked(workloadStopIssued)
			out.stop = w.newStopLocked(p, workloadCleanupStop, act.binding, act.ref)
		} else {
			w.toLocked(workloadFailed)
		}
	}
	w.activation = nil
	close(act.done)
	out.waiters = act.waiting
	return out
}

func (w *workload) clearFailureLocked() {
	w.failures = 0
	w.failedUntil = time.Time{}
	w.failureEvidence = workloadFailureUnproven
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
func (p *dockerProvider) idleExpire(w *workload, binding workloadBindingKey, epoch uint64, run *dockerRun) {
	if run == nil {
		return
	}
	run.idleMu.RLock()
	if run.idleOff.Load() {
		run.idleMu.RUnlock()
		return
	}
	if !w.claimIdleStop(binding, epoch) {
		run.idleMu.RUnlock()
		return
	}
	run.idleMu.RUnlock()
	if !run.track(func(ctx context.Context) { p.performStop(ctx, w, binding, epoch, run) }) {
		w.revokeIdleStop(binding, epoch)
	}
}

func (w *workload) claimIdleStop(binding workloadBindingKey, epoch uint64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.phase != workloadReady || w.binding == nil || w.binding.key != binding ||
		w.idleEpoch != epoch || w.binding.activity.active > 0 || w.retired {
		return false
	}
	w.idle = nil
	w.toLocked(workloadStopPending)
	return true
}

func (w *workload) revokeIdleStop(binding workloadBindingKey, epoch uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.phase == workloadStopPending && w.binding != nil && w.binding.key == binding && w.idleEpoch == epoch {
		w.toLocked(workloadReady)
	}
}

// performStop issues the idle stop unless a request revoked it first. Durable
// ownership survives cancellation; restart resumes an interrupted attempt.
func (p *dockerProvider) performStop(ctx context.Context, w *workload, binding workloadBindingKey, epoch uint64, run *dockerRun) {
	run.idleMu.RLock()
	if run.idleOff.Load() {
		run.idleMu.RUnlock()
		w.revokeIdleStop(binding, epoch)
		return
	}
	w.mu.Lock()
	if w.phase != workloadStopPending || w.binding == nil || w.binding.key != binding || w.idleEpoch != epoch {
		w.mu.Unlock()
		run.idleMu.RUnlock()
		return
	}
	if w.retired {
		w.toLocked(workloadReady)
		w.mu.Unlock()
		run.idleMu.RUnlock()
		return
	}
	w.toLocked(workloadStopIssued)
	stop := w.newStopLocked(p, workloadIdleStop, w.binding.key, w.containerRefLocked())
	w.mu.Unlock()
	run.idleMu.RUnlock()
	p.runOwnedStop(ctx, w, stop)
}

func (w *workload) newStopLocked(p *dockerProvider, kind workloadStopKind, binding workloadBindingKey, ref string) *workloadStop {
	p.invalidateWorkloadObservationsLocked(w)
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

func (p *dockerProvider) attemptOwnedStop(ctx context.Context, w *workload, stop *workloadStop) workloadStopAttempt {
	stopRef := w.callRef(stop.binding, stop.ref)
	sctx, cancel := context.WithTimeout(ctx, workloadStopTimeout)
	err := p.client.StopContainer(sctx, stopRef)
	cancel()
	if err == nil {
		return workloadStopAttempt{result: workloadStopSucceeded}
	}
	if docker.LifecycleContainerMissing(err) {
		return workloadStopAttempt{result: workloadStopSucceeded, stopErr: err}
	}
	if !docker.LifecycleOutcomeAmbiguous(err) {
		return workloadStopAttempt{result: workloadStopRejected, stopErr: err}
	}

	inspectRef := w.callRef(stop.binding, stop.ref)
	ictx, icancel := context.WithTimeout(ctx, workloadProbeTimeout)
	insp, inspectErr := p.client.InspectContainer(ictx, inspectRef)
	icancel()
	if inspectErr == nil && !insp.Running {
		return workloadStopAttempt{result: workloadStopSucceeded, stopErr: err}
	}
	if docker.LifecycleContainerMissing(inspectErr) {
		return workloadStopAttempt{result: workloadStopSucceeded, stopErr: err, inspectErr: inspectErr}
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
	owner, owned := w.stopOwnershipLocked(stop)
	if !owned {
		w.mu.Unlock()
		return workloadStopObsolete
	}
	if attempt.result == workloadStopAmbiguous {
		stop.issued = false
		stop.uncertain = true
		result := w.unsettleStopLocked()
		w.mu.Unlock()
		return result
	}
	if attempt.result == workloadStopRejected && stop.uncertain {
		stop.issued = false
		result := w.unsettleStopLocked()
		w.mu.Unlock()
		return result
	}
	if !stop.terminal {
		p.invalidateWorkloadObservationsLocked(w)
		stop.terminal = true
		stop.result = attempt.result
	}
	result := stop.result
	w.mu.Unlock()

	registry := p.currentMutationRegistry()
	var persistErr error
	if registry == nil {
		persistErr = errors.New("mutation registry is unavailable")
	} else {
		persistErr = registry.delete(owner.containerID)
	}

	w.mu.Lock()
	if !owner.currentLocked(w) {
		w.mu.Unlock()
		return workloadStopObsolete
	}
	if persistErr != nil {
		stop.issued = false
		apply := w.unsettleStopLocked()
		w.mu.Unlock()
		service := strconv.Quote(owner.service)
		log.Printf("statute: docker: workload %s: persist stop settlement: %s", service, strconv.Quote(persistErr.Error()))
		return apply
	}
	stop.issued = false
	// Terminal evidence and durable settlement are separate transitions. Fence
	// generations derived while the WAL deletion was in flight.
	p.invalidateWorkloadObservationsLocked(w)
	p.invalidateStoppedGeneration(owner.service, owner.bindingKey, result)
	w.settleStopLocked(p, stop, result)
	w.mu.Unlock()
	p.scheduleReconcile()
	return workloadStopSettled
}

func (w *workload) unsettleStopLocked() workloadStopApply {
	if w.phase == workloadStopIssued {
		w.toLocked(workloadStopUnknown)
	}
	return workloadStopUnsettled
}

func (w *workload) settleStopLocked(p *dockerProvider, stop *workloadStop, result workloadStopResult) {
	if stop.kind == workloadCleanupStop {
		if result == workloadStopSucceeded {
			w.failureEvidence = workloadFailureStopped
		} else {
			w.failureEvidence = workloadFailureUnproven
		}
		w.toLocked(workloadFailed)
	} else if result == workloadStopSucceeded {
		w.toLocked(workloadDormant)
	} else {
		w.toLocked(workloadReady)
		w.reserveWaitersLocked(&stop.workloadWait)
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
	if err := p.persistOwnedStop(w, stop); err != nil {
		result := w.deferOwnedStop(stop)
		if result != workloadStopObsolete {
			service := strconv.Quote(w.service)
			log.Printf("statute: docker: workload %s: persist stop intent: %s", service, strconv.Quote(err.Error()))
		}
		return result
	}
	terminalResult, terminal, owned := w.stopResult(stop)
	if !owned {
		return workloadStopObsolete
	}
	if terminal {
		return w.applyStopAttempt(p, stop, workloadStopAttempt{result: terminalResult})
	}
	attempt := p.attemptOwnedStop(ctx, w, stop)
	if attempt.result == workloadStopAmbiguous {
		p.markOwnedStopUncertain(w, stop)
	}
	result := w.applyStopAttempt(p, stop, attempt)
	if result != workloadStopUnsettled || logUnsettled {
		p.logStopAttempt(w, stop, attempt, result)
	}
	return result
}

func (p *dockerProvider) persistOwnedStop(w *workload, stop *workloadStop) error {
	w.mu.Lock()
	owner, owned := w.stopOwnershipLocked(stop)
	if !owned {
		w.mu.Unlock()
		return errors.New("stop is obsolete")
	}
	if stop.persisted {
		w.mu.Unlock()
		return nil
	}
	record := mutationRecord{
		ContainerID:   owner.containerID,
		ContainerName: owner.containerName,
		Service:       owner.service,
		Kind:          mutationRecordKindForStop(stop.kind),
		State:         mutationRecordPrepared,
	}
	w.mu.Unlock()

	registry := p.currentMutationRegistry()
	if registry == nil {
		return errors.New("mutation registry is unavailable")
	}
	if err := registry.put(record); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !owner.currentLocked(w) {
		return errors.New("stop is obsolete")
	}
	stop.persisted = true
	return nil
}

func (p *dockerProvider) markOwnedStopUncertain(w *workload, stop *workloadStop) {
	w.mu.Lock()
	if w.stop != stop || w.binding == nil || w.binding.key != stop.binding {
		w.mu.Unlock()
		return
	}
	containerID := w.binding.containerID
	w.mu.Unlock()
	registry := p.currentMutationRegistry()
	if registry != nil {
		if err := registry.markUncertain(containerID); err != nil {
			service := strconv.Quote(w.service)
			log.Printf("statute: docker: workload %s: persist stop uncertainty: %s", service, strconv.Quote(err.Error()))
		}
	}
}

func (w *workload) deferOwnedStop(stop *workloadStop) workloadStopApply {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stop != stop || w.binding == nil || w.binding.key != stop.binding {
		return workloadStopObsolete
	}
	stop.issued = false
	if w.phase == workloadStopIssued {
		w.toLocked(workloadStopUnknown)
	}
	return workloadStopUnsettled
}

func (w *workload) stopResult(stop *workloadStop) (workloadStopResult, bool, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stop != stop {
		return workloadStopSucceeded, false, false
	}
	if !stop.terminal {
		return workloadStopSucceeded, false, true
	}
	return stop.result, true, true
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
		stopErr := renderedNone
		if attempt.stopErr != nil {
			stopErr = strconv.Quote(attempt.stopErr.Error())
		}
		inspect := renderedNone
		if attempt.inspectErr != nil {
			inspect = strconv.Quote(attempt.inspectErr.Error())
		}
		log.Printf("statute: docker: workload %s: stop outcome unknown: stop: %s; inspect: %s", service, stopErr, inspect)
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
	if t == nil || t.workloadMutations[service] != p.currentMutationVersion(service) || t.workloadBindings[service] != binding {
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
	if t == nil || t.workloadMutations[service] != p.currentMutationVersion(service) ||
		t.workloadBindings[service] != binding || t.workloadRevisions[service] != revision {
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

func (p *dockerProvider) idleStopsEnabled() bool {
	p.lifecycleMu.Lock()
	r := p.current
	p.lifecycleMu.Unlock()
	return r != nil && !r.idleOff.Load()
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

func (p *dockerProvider) restoreMutationRecords(records []mutationRecord) {
	p.workloadMu.Lock()
	defer p.workloadMu.Unlock()
	for _, record := range records {
		if p.hasMutationOwnerLocked(record.ContainerID) {
			continue
		}
		binding := p.nextWorkloadBindingLocked()
		stop := &workloadStop{
			kind:       workloadStopKindForRecord(record.Kind),
			binding:    binding,
			ref:        record.ContainerID,
			uncertain:  true,
			persisted:  true,
			converging: false,
		}
		stop.done = make(chan struct{})
		w := &workload{
			service:    record.Service,
			policy:     p.cfg.Workloads[record.Service],
			phase:      workloadStopUnknown,
			binding:    &workloadBinding{key: binding, container: record.ContainerName, containerID: record.ContainerID},
			hadBinding: true,
			retired:    true,
			stop:       stop,
		}
		p.retiredMutations = append(p.retiredMutations, w)
	}
}

func (p *dockerProvider) hasMutationOwnerLocked(containerID string) bool {
	for _, w := range p.workloadEntries {
		if workloadOwnsContainerMutation(w, containerID) {
			return true
		}
	}
	for _, w := range p.retiredMutations {
		if workloadOwnsContainerMutation(w, containerID) {
			return true
		}
	}
	return false
}

func workloadOwnsContainerMutation(w *workload, containerID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stop != nil && w.binding != nil && w.binding.containerID == containerID
}

type workloadObservationTickets map[*workload]uint64

func (p *dockerProvider) captureWorkloadObservationTickets() workloadObservationTickets {
	p.workloadMu.Lock()
	defer p.workloadMu.Unlock()
	tickets := make(workloadObservationTickets, len(p.workloadEntries)+len(p.retiredMutations))
	for _, w := range p.workloadEntries {
		w.mu.Lock()
		tickets[w] = w.observationEpoch
		w.mu.Unlock()
	}
	for _, w := range p.retiredMutations {
		w.mu.Lock()
		tickets[w] = w.observationEpoch
		w.mu.Unlock()
	}
	return tickets
}

func (tickets workloadObservationTickets) currentLocked(w *workload) bool {
	epoch, captured := tickets[w]
	return !captured || epoch == w.observationEpoch
}

func (tickets workloadObservationTickets) allCurrentLocked() bool {
	for w, epoch := range tickets {
		w.mu.Lock()
		current := epoch == w.observationEpoch
		w.mu.Unlock()
		if !current {
			return false
		}
	}
	return true
}

// updateWorkloads reconciles the registry with one derived generation: it
// creates entries for newly covered services, feeds each entry the observed
// container state, and retires entries whose grant disappeared. The policy
// applies only to a one-to-one candidate service and container pair. A retired
// mutation still quarantines every service contributed by its container until
// the mutation settles.
func (p *dockerProvider) updateWorkloads(services []docker.Service, containers []docker.Container, topology workloadCandidateTopology, tickets workloadObservationTickets) (retiredMutationQuarantine, bool) {
	p.workloadMu.Lock()
	defer p.workloadMu.Unlock()
	if !tickets.allCurrentLocked() {
		return retiredMutationQuarantine{}, false
	}
	eligible, seen := p.workloadEligibilityLocked(services, topology)
	if !p.retireMissingLocked(seen, tickets) {
		return retiredMutationQuarantine{}, false
	}
	for i := range services {
		if eligible[i] && !p.prepareWorkloadObservationLocked(&services[i], tickets) {
			return retiredMutationQuarantine{}, false
		}
	}
	if !p.reconcileRetiredMutationObservationsLocked(containers, tickets) || !tickets.allCurrentLocked() {
		return retiredMutationQuarantine{}, false
	}
	return p.retiredMutationQuarantineLocked(), true
}

func (p *dockerProvider) workloadEligibilityLocked(services []docker.Service, topology workloadCandidateTopology) ([]bool, map[string]bool) {
	seen := make(map[string]bool, len(p.cfg.Workloads))
	eligible := make([]bool, len(services))
	for i := range services {
		svc := &services[i]
		if p.workloadEligibleLocked(svc, topology) {
			seen[svc.Name] = true
			eligible[i] = true
		}
	}
	return eligible, seen
}

func (p *dockerProvider) prepareWorkloadObservationLocked(svc *docker.Service, tickets workloadObservationTickets) bool {
	w := p.workloadEntries[svc.Name]
	if w != nil {
		w.mu.Lock()
		if !tickets.currentLocked(w) {
			w.mu.Unlock()
			return false
		}
		if w.ownsIssuedMutationForOtherContainerLocked(svc) {
			w = p.detachMutationOwnerHeldLocked(w, svc.Name)
		} else if w.retired && w.hasUnsettledStopLocked() {
			w.mu.Unlock()
			return true
		} else {
			w.unretireLocked()
			p.observeWorkloadLocked(w, svc)
			w.mu.Unlock()
			return true
		}
	}
	if w == nil {
		if p.workloadEntries == nil {
			p.workloadEntries = map[string]*workload{}
		}
		w = &workload{service: svc.Name, policy: p.cfg.Workloads[svc.Name]}
		p.workloadEntries[svc.Name] = w
	}
	w.mu.Lock()
	w.unretireLocked()
	p.observeWorkloadLocked(w, svc)
	w.mu.Unlock()
	return true
}

// detachMutationOwnerHeldLocked separates an immutable predecessor mutation
// from the current grant. The successor inherits replacement history and backoff.
func (p *dockerProvider) detachMutationOwnerHeldLocked(old *workload, service string) *workload {
	old.retired = true
	old.stopIdleLocked()
	failures := old.failures
	failedUntil := old.failedUntil
	old.mu.Unlock()
	p.retiredMutations = append(p.retiredMutations, old)
	fresh := &workload{
		service: service, policy: p.cfg.Workloads[service], hadBinding: old.hadBinding,
		failures: failures, failedUntil: failedUntil,
	}
	if failures > 0 {
		fresh.phase = workloadFailed
	}
	p.workloadEntries[service] = fresh
	return fresh
}

func (w *workload) hasUnsettledStopLocked() bool {
	return w.stop != nil && (w.phase == workloadStopIssued || w.phase == workloadStopUnknown)
}

func (p *dockerProvider) workloadEligibleLocked(svc *docker.Service, topology workloadCandidateTopology) bool {
	if _, ok := p.cfg.Workloads[svc.Name]; !ok {
		return false
	}
	if svc.Contributors != 1 {
		p.warn([]string{fmt.Sprintf("service %q: on-demand workload policy needs one contributing container, found %d; policy not applied", svc.Name, svc.Contributors)})
		return false
	}
	if contributors := p.availableCandidateContributorsLocked(svc, topology); contributors != 1 {
		p.warn([]string{fmt.Sprintf("service %q: on-demand workload policy needs one candidate container, found %d; policy not applied", svc.Name, contributors)})
		return false
	}
	if len(topology.servicesFor(svc.Container, svc.ContainerID)) != 1 {
		p.warn([]string{fmt.Sprintf("service %q: container %q contributes more than one service and a stop acts on all of them; on-demand policy not applied", svc.Name, svc.Container)})
		return false
	}
	return true
}

// availableCandidateContributorsLocked excludes only an unextractable
// predecessor whose immutable identity is already owned by an unsettled stop.
// Successfully extracted contributors remain ambiguous even when quarantined.
func (p *dockerProvider) availableCandidateContributorsLocked(svc *docker.Service, topology workloadCandidateTopology) int {
	contributors := topology.contributors[svc.Name]
	excluded := map[workloadContainerRef]bool{}
	exclude := func(w *workload) {
		ref, ok := mutationCandidateForOtherContainer(w, svc)
		if !ok || excluded[ref] || !slices.Contains(topology.servicesFor(ref.name, ref.id), svc.Name) {
			return
		}
		excluded[ref] = true
		contributors--
	}
	for _, current := range p.workloadEntries {
		exclude(current)
	}
	for _, retired := range p.retiredMutations {
		exclude(retired)
	}
	return contributors
}

func mutationCandidateForOtherContainer(w *workload, svc *docker.Service) (workloadContainerRef, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.ownsIssuedMutationForOtherContainerLocked(svc) {
		return workloadContainerRef{}, false
	}
	return workloadContainerRef{name: w.binding.container, id: w.binding.containerID}, true
}

type workloadContainerRef struct {
	name string
	id   string
}

type retiredMutationQuarantine struct {
	containers []workloadContainerRef
	routes     []docker.RouteClaim
}

func (q retiredMutationQuarantine) matches(c docker.Container) bool {
	return slices.ContainsFunc(q.containers, func(ref workloadContainerRef) bool { return ref.matches(c) })
}

func (r workloadContainerRef) matches(c docker.Container) bool {
	return r.matchesIdentity(c.Name, c.ID)
}

func (r workloadContainerRef) matchesIdentity(name, id string) bool {
	if r.id != "" && id != "" {
		return r.id == id
	}
	return r.name != "" && r.name == name
}

// retiredMutationQuarantinesLocked maps a retired stop's container-wide
// authority onto every service the same immutable container currently
// contributes. Grant retirement prevents new lifecycle calls; it does not
// make an already-issued stop safe to route around. p.workloadMu must be held.
func (p *dockerProvider) retiredMutationQuarantineLocked() retiredMutationQuarantine {
	refs := p.retiredMutationContainerRefsLocked()
	if len(refs) == 0 {
		return retiredMutationQuarantine{}
	}
	return retiredMutationQuarantine{containers: refs}
}

func (p *dockerProvider) retiredMutationContainerRefsLocked() []workloadContainerRef {
	var refs []workloadContainerRef
	for _, w := range p.workloadEntries {
		refs = p.appendRetiredMutationRefLocked(refs, w)
	}
	kept := p.retiredMutations[:0]
	for _, w := range p.retiredMutations {
		before := len(refs)
		refs = p.appendRetiredMutationRefLocked(refs, w)
		if len(refs) > before {
			kept = append(kept, w)
		}
	}
	p.retiredMutations = kept
	return refs
}

func (p *dockerProvider) reconcileRetiredMutationObservationsLocked(containers []docker.Container, tickets workloadObservationTickets) bool {
	for _, w := range p.workloadEntries {
		if !p.reconcileRetiredMutationObservationLocked(w, containers, tickets) {
			return false
		}
	}
	for _, w := range p.retiredMutations {
		if !p.reconcileRetiredMutationObservationLocked(w, containers, tickets) {
			return false
		}
	}
	return true
}

func (p *dockerProvider) reconcileRetiredMutationObservationLocked(w *workload, containers []docker.Container, tickets workloadObservationTickets) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !tickets.currentLocked(w) {
		return false
	}
	if !w.retired || !w.hasUnsettledStopLocked() || w.binding == nil || w.binding.containerID == "" {
		return true
	}
	if w.stop.issued {
		return true
	}
	for _, container := range containers {
		if container.ID == w.binding.containerID {
			if container.Running {
				return true
			}
			p.recordObservedStopLocked(w)
			return true
		}
	}
	p.recordObservedStopLocked(w)
	return true
}

func (p *dockerProvider) recordObservedStopLocked(w *workload) {
	stop := w.stop
	if !stop.terminal {
		p.invalidateWorkloadObservationsLocked(w)
	}
	stop.terminal = true
	stop.result = workloadStopSucceeded
	p.ensureStopConvergenceLocked(w, stop)
}

func (p *dockerProvider) appendRetiredMutationRefLocked(refs []workloadContainerRef, w *workload) []workloadContainerRef {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.retired || w.stop == nil || w.binding == nil || w.stop.binding != w.binding.key ||
		(w.phase != workloadStopIssued && w.phase != workloadStopUnknown) {
		return refs
	}
	p.ensureStopConvergenceLocked(w, w.stop)
	return append(refs, workloadContainerRef{name: w.binding.container, id: w.binding.containerID})
}

// unretire restores the grant on a retained entry. A retained phase can be
// stale: the container was removed while ready, so running now proves
// nothing. A stale ready or stop-pending resets to dormant, keeping the
// backoff bookkeeping, and the following observation re-proves readiness
// through the observe gate and re-arms the idle timer.
func (w *workload) unretireLocked() {
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
func (p *dockerProvider) retireMissingLocked(seen map[string]bool, tickets workloadObservationTickets) bool {
	for name, w := range p.workloadEntries {
		if seen[name] {
			continue
		}
		w.mu.Lock()
		if !tickets.currentLocked(w) {
			w.mu.Unlock()
			return false
		}
		alreadyRetired := w.retired
		w.retired = true
		w.stopIdleLocked()
		phase := w.phase
		w.mu.Unlock()
		if !alreadyRetired && (phase == workloadReady || phase == workloadStarting) {
			p.warn([]string{fmt.Sprintf("service %q: on-demand grant removed; its container is left as it is", name)})
		}
	}
	return true
}

// observeWorkload feeds one discovery observation into the state machine. Once
// this process established lifecycle authority, a replacement running container
// or a stopped-to-running transition enters the readiness gate observe-only and
// clears backoff. Repeated running observations prove no repair. An externally
// stopped ready workload reconciles to dormant; in-flight requests fail through
// the normal proxy error path.
func (p *dockerProvider) observeWorkloadLocked(w *workload, svc *docker.Service) {
	replaced := w.binding == nil && w.hadBinding
	if p.bindWorkloadContainerLocked(w, svc) {
		replaced = true
		log.Printf("statute: docker: workload %q: container binding replaced", w.service)
	}
	if svc.Running {
		p.observeRunningWorkloadLocked(w, replaced)
		return
	}
	p.observeStoppedWorkloadLocked(w)
}

func (p *dockerProvider) observeRunningWorkloadLocked(w *workload, replaced bool) {
	switch w.phase {
	case workloadStopIssued, workloadStopUnknown:
		p.ensureStopConvergenceLocked(w, w.stop)
	case workloadDormant:
		if _, err := p.beginActivationLocked(w, true); err == nil {
			log.Printf("statute: docker: workload %q: found running, establishing readiness", w.service)
		}
	case workloadFailed:
		if !replaced && w.failureEvidence != workloadFailureStopped {
			return
		}
		w.clearFailureLocked()
		if _, err := p.beginActivationLocked(w, true); err == nil {
			log.Printf("statute: docker: workload %q: external repair found, establishing readiness", w.service)
		}
	case workloadStarting, workloadReady, workloadStopPending:
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
	case workloadStopIssued, workloadStopUnknown:
		if w.stop != nil {
			if w.stop.issued {
				return
			}
			p.recordObservedStopLocked(w)
		}
	case workloadFailed:
		w.failureEvidence = workloadFailureStopped
	default:
		// Dormant already agrees; starting resolves through its own in-flight
		// operation.
	}
}

func (p *dockerProvider) markMutationSettled(service string) {
	p.generationMu.Lock()
	p.markMutationSettledLocked(service)
	p.generationMu.Unlock()
}

func (p *dockerProvider) markMutationSettledLocked(service string) {
	if p.mutationVersions == nil {
		p.mutationVersions = make(map[string]uint64)
	}
	p.mutationVersions[service]++
}

func (p *dockerProvider) invalidateStoppedGeneration(service string, binding workloadBindingKey, result workloadStopResult) {
	// Rejection leaves the running pool valid. Observation revisions still
	// fence generations derived before settlement.
	if result != workloadStopSucceeded {
		return
	}
	p.generationMu.Lock()
	defer p.generationMu.Unlock()
	if p.srv != nil {
		table := p.srv.dynamic.Load()
		if table != nil {
			if current, exists := table.workloadBindings[service]; exists && current != binding {
				return
			}
		}
	}
	p.markMutationSettledLocked(service)
}

func (p *dockerProvider) currentMutationVersion(service string) uint64 {
	p.generationMu.Lock()
	defer p.generationMu.Unlock()
	return p.mutationVersions[service]
}

// resumeWorkloadLifecycles restores provider-run work cancelled by shutdown.
func (p *dockerProvider) resumeWorkloadLifecycles() {
	p.workloadMu.Lock()
	defer p.workloadMu.Unlock()
	for _, w := range p.workloadEntries {
		p.resumeWorkloadLifecycleLocked(w, true)
	}
	for _, w := range p.retiredMutations {
		p.resumeWorkloadLifecycleLocked(w, false)
	}
}

func (p *dockerProvider) resumeWorkloadLifecycleLocked(w *workload, armIdle bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if armIdle && !w.retired && w.phase == workloadReady {
		w.armIdleLocked(p)
	}
	if w.stop != nil && (w.phase == workloadStopIssued || w.phase == workloadStopUnknown) {
		p.ensureStopConvergenceLocked(w, w.stop)
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
	for _, w := range p.retiredMutations {
		w.mu.Lock()
		w.stopIdleLocked()
		w.mu.Unlock()
	}
}
