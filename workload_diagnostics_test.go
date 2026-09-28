package statute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"statute.kjanat.dev/internal/docker"
	"statute.kjanat.dev/resolved"
)

func diagnosticUnitProvider(t *testing.T) (*dockerProvider, *workload) {
	t.Helper()
	p := &dockerProvider{cfg: &resolved.Docker{Workloads: map[string]resolved.Workload{"wl": testWorkloadPolicy()}}}
	p.workloadMu.Lock()
	if !p.prepareWorkloadObservationLocked(&docker.Service{Name: "wl", Container: "secret-container", ContainerID: "secret-id"}, nil) {
		t.Fatal("initial workload observation rejected")
	}
	p.workloadMu.Unlock()
	return p, p.workloadFor("wl")
}

func diagnosticService(t *testing.T, p *dockerProvider) workloadServiceSnapshot {
	t.Helper()
	snapshot := p.snapshotWorkloads()
	if len(snapshot.Services) != 1 || snapshot.Services[0].Service != "wl" {
		t.Fatalf("services = %+v, want configured wl", snapshot.Services)
	}
	return snapshot.Services[0]
}

func diagnosticActivation(p *dockerProvider, w *workload, observe bool) *workloadActivation {
	w.mu.Lock()
	defer w.mu.Unlock()
	act := &workloadActivation{observe: observe, binding: w.binding.key, started: time.Now().Add(-time.Second)}
	act.done = make(chan struct{})
	w.phase, w.activation = workloadStarting, act
	p.recordWorkloadEvent(w.service, workloadActivationAttemptEvent)
	return act
}

func TestWorkloadDiagnosticsEmptyAndConfiguredServices(t *testing.T) {
	var absent *dockerProvider
	encoded, err := json.Marshal(absent.snapshotWorkloads())
	if err != nil || string(encoded) != `{"services":[],"orphaned_mutations":[]}` {
		t.Fatalf("absent diagnostics = %s, %v", encoded, err)
	}
	p := &dockerProvider{cfg: &resolved.Docker{Workloads: map[string]resolved.Workload{"z": {}, "a": {}}}}
	snapshot := p.snapshotWorkloads()
	if len(snapshot.Services) != 2 || snapshot.Services[0].Service != "a" || snapshot.Services[1].Service != "z" {
		t.Fatalf("configured services are not stable and sorted: %+v", snapshot)
	}
	if snapshot.Services[0].Owners == nil || snapshot.OrphanedMutations == nil {
		t.Fatal("empty diagnostic collections must be arrays")
	}
}

func TestWorkloadDiagnosticsSingleFlightAndWaiters(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	t.Cleanup(backend.Close)
	policy := testWorkloadPolicy()
	policy.IdleAfter = time.Hour
	policy.Readiness.Mode = resolved.ReadinessDockerHealth
	p, daemon, router := workloadFixture(t, policy, backend.URL)
	daemon.mu.Lock()
	daemon.find("wl-1").health = "starting"
	daemon.mu.Unlock()
	const requests = 8
	responses := make(chan int, requests)
	for range requests {
		go func() {
			responses <- runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil)).Code
		}()
	}
	waitDiagnosticWaiters(t, p, requests)
	service := diagnosticService(t, p)
	if service.ActivationAttempts != 1 || service.ActivationFailures != 0 || service.Owners[0].Phase != "starting" {
		t.Fatalf("single-flight diagnostics = %+v", service)
	}
	exerciseDiagnosticCancelledWaiter(t, p, router, requests)
	daemon.mu.Lock()
	daemon.find("wl-1").health = "healthy"
	daemon.mu.Unlock()
	for range requests {
		if status := waitStatus(t, responses, 5*time.Second, "activation did not release waiter"); status != http.StatusOK {
			t.Fatalf("request = %d", status)
		}
	}
	service = diagnosticService(t, p)
	assertDiagnosticCompletedActivation(t, service)
}

func assertDiagnosticCompletedActivation(t *testing.T, service workloadServiceSnapshot) {
	t.Helper()
	owner := service.Owners[0]
	if service.ActivationAttempts != 1 || owner.ActivationWaiters != 0 || owner.LastActivationSeconds <= 0 || owner.LastFailureAt != nil {
		t.Fatalf("completed activation diagnostics = %+v %+v", service, owner)
	}
}

func exerciseDiagnosticCancelledWaiter(t *testing.T, p *dockerProvider, router http.Handler, waiting int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	response := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil).WithContext(ctx)
		response <- runRequest(t, router, req).Code
	}()
	waitDiagnosticWaiters(t, p, waiting+1)
	cancel()
	if status := waitStatus(t, response, time.Second, "cancelled activation waiter did not leave"); status != http.StatusServiceUnavailable {
		t.Fatalf("cancelled waiter response = %d", status)
	}
	waitDiagnosticWaiters(t, p, waiting)
	if service := diagnosticService(t, p); service.ActivationAttempts != 1 || service.ActivationFailures != 0 {
		t.Fatalf("waiter cancellation changed attempt/failure counts: %+v", service)
	}
}

func waitDiagnosticWaiters(t *testing.T, p *dockerProvider, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		service := diagnosticService(t, p)
		if len(service.Owners) == 1 && service.Owners[0].ActivationWaiters == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("activation waiters did not attach")
}

func TestWorkloadDiagnosticsFailedTrackingDoesNotCount(t *testing.T) {
	p, w := diagnosticUnitProvider(t)
	w.mu.Lock()
	_, err := p.beginActivationLocked(w, false)
	w.mu.Unlock()
	if err == nil || diagnosticService(t, p).ActivationAttempts != 0 {
		t.Fatal("untracked activation counted as an attempt")
	}
}

func TestWorkloadDiagnosticsRuntimeFailureStages(t *testing.T) {
	for _, startFailure := range []bool{false, true} {
		name := "readiness-timeout"
		if startFailure {
			name = "start-failed"
		}
		t.Run(name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			t.Cleanup(backend.Close)
			policy := testWorkloadPolicy()
			policy.Readiness.Mode = resolved.ReadinessDockerHealth
			policy.ReadyTimeout = 25 * time.Millisecond
			p, daemon, router := workloadFixture(t, policy, backend.URL)
			daemon.mu.Lock()
			daemon.failStart = startFailure
			daemon.find("wl-1").health = "starting"
			daemon.mu.Unlock()
			if rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil)); rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("failed activation = %d", rec.Code)
			}
			waitWorkloadPhase(t, p, workloadFailed)
			service := diagnosticService(t, p)
			if service.ActivationAttempts != 1 || service.ActivationFailures != 1 || service.IdleStops != 0 || service.Owners[0].LastFailureReason != name {
				t.Fatalf("failed activation diagnostics = %+v %+v", service, service.Owners)
			}
		})
	}
}

func TestWorkloadDiagnosticsSafeFailureReasons(t *testing.T) {
	secret := errors.New("http://user:password@docker.internal/containers/secret-id?label=secret-label")
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"start", classifyWorkloadStartFailure(secret), "start-failed"},
		{"start timeout", classifyWorkloadStartFailure(fmt.Errorf("%w: %w", context.DeadlineExceeded, secret)), "start-timeout"},
		{"readiness timeout", classifyWorkloadReadinessFailure(fmt.Errorf("%w: %w", errWorkloadReadinessTimeout, secret)), "readiness-timeout"},
		{"stopped", classifyWorkloadReadinessFailure(errWorkloadStopped), "stopped-before-ready"},
		{"unknown", secret, "activation-failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, w := diagnosticUnitProvider(t)
			act := diagnosticActivation(p, w, false)
			w.settleActivation(p, act, tc.err)
			service := diagnosticService(t, p)
			if service.ActivationFailures != 1 || service.Owners[0].LastFailureReason != tc.want || service.Owners[0].LastFailureAt == nil {
				t.Fatalf("failure diagnostic = %+v %+v", service, service.Owners)
			}
			assertDiagnosticsRedacted(t, p)
		})
	}
}

func assertDiagnosticsRedacted(t *testing.T, p *dockerProvider) {
	t.Helper()
	encoded, err := json.Marshal(p.snapshotWorkloads())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret", "password", "docker.internal", "http://", "user:"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("diagnostics leaked %q: %s", secret, encoded)
		}
	}
}

func TestWorkloadDiagnosticsCancelledStaleAndSuperseded(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"shutdown", classifyWorkloadStartFailure(context.Canceled)},
		{"stale observation", classifyWorkloadReadinessFailure(errWorkloadStopped)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, w := diagnosticUnitProvider(t)
			act := diagnosticActivation(p, w, true)
			w.settleActivation(p, act, tc.err)
			service := diagnosticService(t, p)
			if service.ActivationFailures != 0 || service.Owners[0].LastFailureAt != nil || service.Owners[0].LastActivationSeconds <= 0 {
				t.Fatalf("non-failure outcome contaminated diagnostics: %+v %+v", service, service.Owners)
			}
		})
	}
	p, w := diagnosticUnitProvider(t)
	act := diagnosticActivation(p, w, true)
	p.workloadMu.Lock()
	w.mu.Lock()
	p.bindWorkloadContainerLocked(w, &docker.Service{Container: "successor", ContainerID: "different-id"})
	w.mu.Unlock()
	p.workloadMu.Unlock()
	if out := w.settleActivation(p, act, errors.New("stale secret failure")); !out.superseded {
		t.Fatal("old activation was not superseded")
	}
	owner := diagnosticService(t, p).Owners[0]
	if owner.LastFailureReason != "" || owner.LastActivationSeconds != 0 {
		t.Fatalf("superseded completion changed successor: %+v", owner)
	}
}

func TestWorkloadDiagnosticsReplacementAndRegrant(t *testing.T) {
	p, w := diagnosticUnitProvider(t)
	act := diagnosticActivation(p, w, true)
	w.settleActivation(p, act, classifyWorkloadReadinessFailure(errWorkloadReadinessTimeout))
	before := diagnosticService(t, p)
	p.workloadMu.Lock()
	p.retireMissingLocked(nil, nil)
	if !p.prepareWorkloadObservationLocked(&docker.Service{Name: "wl", Container: "secret-container", ContainerID: "secret-id"}, nil) {
		t.Fatal("same binding regrant failed")
	}
	p.workloadMu.Unlock()
	after := diagnosticService(t, p)
	if after.Owners[0].Incarnation != before.Owners[0].Incarnation || after.Owners[0].LastFailureReason != before.Owners[0].LastFailureReason || after.ActivationFailures != 1 {
		t.Fatalf("same-binding regrant lost history: %+v", after)
	}
	p.workloadMu.Lock()
	w.mu.Lock()
	p.bindWorkloadContainerLocked(w, &docker.Service{Container: "successor", ContainerID: "different-id"})
	w.mu.Unlock()
	p.workloadMu.Unlock()
	after = diagnosticService(t, p)
	if after.Owners[0].Incarnation == before.Owners[0].Incarnation || after.Owners[0].LastFailureReason != "" || after.ActivationFailures != 1 {
		t.Fatalf("replacement retention is incorrect: %+v %+v", after, after.Owners)
	}
}

func diagnosticMutationProvider(t *testing.T, kind workloadStopKind) (*dockerProvider, *workload, *workloadStop) {
	t.Helper()
	p, w := diagnosticUnitProvider(t)
	registry, err := openMutationRegistry(t.TempDir(), "secret-endpoint")
	if err != nil {
		t.Fatal(err)
	}
	p.current = &dockerRun{registry: registry, kick: make(chan struct{}, 1)}
	p.current.idleOff.Store(true)
	w.mu.Lock()
	w.phase = workloadStopIssued
	stop := w.newStopLocked(p, kind, w.binding.key, w.binding.ref())
	w.mu.Unlock()
	if err := p.persistOwnedStop(w, stop); err != nil {
		t.Fatal(err)
	}
	return p, w, stop
}

func TestWorkloadDiagnosticsIdleCountsCanonicalSettlement(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kind    workloadStopKind
		results []workloadStopResult
		want    uint64
	}{
		{"success", workloadIdleStop, []workloadStopResult{workloadStopSucceeded}, 1},
		{"rejected", workloadIdleStop, []workloadStopResult{workloadStopRejected}, 0},
		{"cleanup", workloadCleanupStop, []workloadStopResult{workloadStopSucceeded}, 0},
		{"ambiguous rejected succeeded", workloadIdleStop, []workloadStopResult{workloadStopAmbiguous, workloadStopRejected, workloadStopSucceeded}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, w, stop := diagnosticMutationProvider(t, tc.kind)
			for i, result := range tc.results {
				w.applyStopAttempt(p, stop, workloadStopAttempt{result: result})
				if i < len(tc.results)-1 && diagnosticService(t, p).IdleStops != 0 {
					t.Fatal("unresolved stop counted as shutdown")
				}
			}
			w.applyStopAttempt(p, stop, workloadStopAttempt{result: workloadStopSucceeded})
			if got := diagnosticService(t, p).IdleStops; got != tc.want {
				t.Fatalf("idle stops = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestWorkloadDiagnosticsIdleWaitsForDurableDeletion(t *testing.T) {
	p, w, stop := diagnosticMutationProvider(t, workloadIdleStop)
	registry := p.currentMutationRegistry()
	root := registry.root
	registry.root = filepath.Join(t.TempDir(), "missing-directory")
	if got := w.applyStopAttempt(p, stop, workloadStopAttempt{result: workloadStopSucceeded}); got != workloadStopUnsettled {
		t.Fatalf("failed durable deletion = %v", got)
	}
	if diagnosticService(t, p).IdleStops != 0 || !registry.contains("secret-id") {
		t.Fatal("failed deletion counted shutdown or erased durable ownership")
	}
	registry.root = root
	if got := w.applyStopAttempt(p, stop, workloadStopAttempt{result: workloadStopSucceeded}); got != workloadStopSettled {
		t.Fatalf("recovered durable deletion = %v", got)
	}
	if diagnosticService(t, p).IdleStops != 1 || registry.contains("secret-id") {
		t.Fatal("canonical durable settlement did not count exactly once")
	}
}

func TestWorkloadDiagnosticsLastFailureSurvivesSuccessfulRetry(t *testing.T) {
	p, w := diagnosticUnitProvider(t)
	act := diagnosticActivation(p, w, true)
	w.settleActivation(p, act, classifyWorkloadReadinessFailure(errWorkloadReadinessTimeout))
	before := diagnosticService(t, p).Owners[0]
	act = diagnosticActivation(p, w, true)
	w.settleActivation(p, act, nil)
	after := diagnosticService(t, p)
	if after.ActivationFailures != 1 || after.ActivationAttempts != 2 || after.Owners[0].LastFailureReason != before.LastFailureReason || !after.Owners[0].LastFailureAt.Equal(*before.LastFailureAt) {
		t.Fatalf("successful retry lost last failure history: %+v %+v", after, after.Owners)
	}
}

func TestWorkloadDiagnosticsProviderRunRestartPreservesHistory(t *testing.T) {
	p, _, subscriptions := externalWorkloadFixture(t, 0)
	stream := nextExternalSubscription(t, subscriptions)
	close(stream.allow)
	assertExternalWorkloadServes(t, p)
	before := diagnosticService(t, p)
	p.currentRun().stop()
	run, err := p.start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(run.stop)
	after := diagnosticService(t, p)
	if after.ActivationAttempts != before.ActivationAttempts || after.Owners[0].Incarnation != before.Owners[0].Incarnation || after.Owners[0].LastActivationSeconds != before.Owners[0].LastActivationSeconds {
		t.Fatalf("provider-run restart reset history: before %+v, after %+v", before, after)
	}
}

func TestWorkloadDiagnosticsDetachedOwnerPruningPreservesCounters(t *testing.T) {
	p, old, stop := diagnosticMutationProvider(t, workloadIdleStop)
	old.binding.diagnostics.lastFailureReason = "readiness-timeout"
	p.recordWorkloadEvent("wl", workloadActivationAttemptEvent)
	p.recordWorkloadEvent("wl", workloadActivationFailureEvent)
	p.workloadMu.Lock()
	old.mu.Lock()
	fresh := p.detachMutationOwnerHeldLocked(old, "wl")
	fresh.mu.Lock()
	p.newWorkloadBindingLocked(fresh, &docker.Service{Container: "successor", ContainerID: "different-id"})
	fresh.mu.Unlock()
	p.workloadMu.Unlock()
	service := diagnosticService(t, p)
	assertDiagnosticDetachedOwners(t, service)
	if got := old.applyStopAttempt(p, stop, workloadStopAttempt{result: workloadStopSucceeded}); got != workloadStopSettled {
		t.Fatalf("predecessor settlement = %v", got)
	}
	p.workloadMu.Lock()
	p.retiredMutationContainerRefsLocked()
	p.workloadMu.Unlock()
	service = diagnosticService(t, p)
	if len(service.Owners) != 1 || service.ActivationAttempts != 1 || service.ActivationFailures != 1 || service.IdleStops != 1 {
		t.Fatalf("pruning reduced service counters: %+v", service)
	}
}

func assertDiagnosticDetachedOwners(t *testing.T, service workloadServiceSnapshot) {
	t.Helper()
	if len(service.Owners) != 2 || !service.Owners[0].Current || service.Owners[0].LastFailureReason != "" || !service.Owners[1].Retired || service.Owners[1].Current {
		t.Fatalf("detached owners not distinguished: %+v", service.Owners)
	}
}

func TestWorkloadDiagnosticsRecoveryRedactsUnknownService(t *testing.T) {
	p, _, _ := diagnosticMutationProvider(t, workloadIdleStop)
	p.restoreMutationRecords([]mutationRecord{
		{ContainerID: "secret-recovered", ContainerName: "secret-name", Service: "wl", Kind: mutationRecordIdleStop},
		{ContainerID: "secret-orphan", ContainerName: "secret-name", Service: "secret-WAL-service", Kind: mutationRecordIdleStop},
	})
	snapshot := p.snapshotWorkloads()
	if len(snapshot.OrphanedMutations) != 1 || snapshot.OrphanedMutations[0].Phase != "stop-unknown" || !snapshot.OrphanedMutations[0].Retired {
		t.Fatalf("recovered orphan = %+v", snapshot.OrphanedMutations)
	}
	if snapshot.Services[0].ActivationAttempts != 0 || snapshot.Services[0].ActivationFailures != 0 {
		t.Fatal("recovery invented historical counters")
	}
	assertDiagnosticsRedacted(t, p)
	for _, owner := range p.retiredMutations {
		owner.applyStopAttempt(p, owner.stop, workloadStopAttempt{result: workloadStopSucceeded})
	}
	if got := diagnosticService(t, p).IdleStops; got != 1 {
		t.Fatalf("configured recovered shutdowns = %d, want 1", got)
	}
	if len(p.diagnostics.services) != 1 {
		t.Fatal("unconfigured WAL service allocated counter cardinality")
	}
}

func TestWorkloadDiagnosticsExternalTransitions(t *testing.T) {
	p, daemon, subscriptions := externalWorkloadFixture(t, 0)
	stream := nextExternalSubscription(t, subscriptions)
	close(stream.allow)
	assertExternalWorkloadServes(t, p)
	externalRunning(daemon, false)
	mustSync(t, p)
	mustSync(t, p)
	if service := diagnosticService(t, p); service.ExternalStops != 1 || service.ExternalStarts != 0 {
		t.Fatalf("observed stop counters = %+v", service)
	}
	externalRunning(daemon, true)
	mustSync(t, p)
	waitWorkloadPhase(t, p, workloadReady)
	mustSync(t, p)
	service := diagnosticService(t, p)
	if service.ExternalStarts != 1 || service.ExternalStops != 1 || service.ActivationAttempts != 2 {
		t.Fatalf("external adoption counters = %+v", service)
	}
}

func TestWorkloadDiagnosticsConcurrentSnapshots(t *testing.T) {
	p, w := diagnosticUnitProvider(t)
	var workers sync.WaitGroup
	workers.Go(func() {
		for range 100 {
			act := diagnosticActivation(p, w, true)
			w.settleActivation(p, act, classifyWorkloadReadinessFailure(errWorkloadReadinessTimeout))
		}
	})
	workers.Go(func() {
		for range 100 {
			p.workloadMu.Lock()
			w.mu.Lock()
			w.retired = !w.retired
			w.mu.Unlock()
			p.workloadMu.Unlock()
		}
	})
	for range 100 {
		service := diagnosticService(t, p)
		assertDiagnosticSingleAttemptConsistency(t, service)
		owner := service.Owners[0]
		if service.ActivationFailures > 0 && (owner.LastFailureAt == nil || owner.LastFailureReason != "readiness-timeout") {
			t.Fatalf("failure counter preceded owner details: %+v %+v", service, owner)
		}
		if owner.LastFailureAt != nil {
			*owner.LastFailureAt = time.Time{}
		}
	}
	workers.Wait()
	if diagnosticService(t, p).Owners[0].LastFailureAt.IsZero() {
		t.Fatal("snapshot escaped mutable timestamp")
	}
}

func assertDiagnosticSingleAttemptConsistency(t *testing.T, service workloadServiceSnapshot) {
	t.Helper()
	if service.ActivationFailures > service.ActivationAttempts || service.ActivationAttempts-service.ActivationFailures > 1 {
		t.Fatalf("snapshot mixed counter/owner revisions: %+v", service)
	}
}

func TestWorkloadDiagnosticsConcurrentLifecycleSnapshots(t *testing.T) {
	p, daemon, subscriptions := externalWorkloadFixture(t, 0)
	stream := nextExternalSubscription(t, subscriptions)
	close(stream.allow)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sampledRetired := make(chan struct{})
	snapshotResult := make(chan error, 1)
	go func() { snapshotResult <- sampleLifecycleDiagnostics(ctx, p, sampledRetired) }()
	assertExternalWorkloadServes(t, p)
	old := p.workloadFor("wl")
	stopStarted, stopRelease := make(chan struct{}), make(chan struct{})
	var release sync.Once
	releaseStop := func() { release.Do(func() { close(stopRelease) }) }
	defer releaseStop()
	daemon.mu.Lock()
	daemon.stopStarted, daemon.stopRelease = stopStarted, stopRelease
	replacement := daemon.containers[0]
	daemon.mu.Unlock()
	old.mu.Lock()
	old.policy.IdleAfter = time.Millisecond
	old.armIdleLocked(p)
	old.mu.Unlock()
	waitSignal(t, stopStarted, "idle stop did not reach Docker")
	replacement = daemon.recreate(replacement)
	replacement.health = "starting"
	daemon.swap([]fakeDaemonContainer{replacement})
	mustSync(t, p)
	waitSignal(t, sampledRetired, "snapshots did not observe current and retired owners together")
	response := make(chan int, 1)
	go func() {
		response <- runRequest(t, p.srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil)).Code
	}()
	waitDiagnosticCurrentWaiter(t, p)
	daemon.mu.Lock()
	daemon.find("wl-1").health = "healthy"
	daemon.mu.Unlock()
	mustSync(t, p)
	if status := waitStatus(t, response, 5*time.Second, "successor readiness did not release request"); status != http.StatusOK {
		t.Fatalf("successor response = %d", status)
	}
	releaseStop()
	waitRetiredWorkloadDormant(t, old)
	mustSync(t, p)
	p.currentRun().stop()
	cancel()
	if err := <-snapshotResult; err != nil {
		t.Fatal(err)
	}
	service := diagnosticService(t, p)
	if len(service.Owners) != 1 || service.IdleStops != 1 || service.ActivationAttempts != 2 {
		t.Fatalf("retirement/shutdown snapshot = %+v %+v", service, service.Owners)
	}
}

func waitDiagnosticCurrentWaiter(t *testing.T, p *dockerProvider) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, owner := range diagnosticService(t, p).Owners {
			if owner.Current && owner.ActivationWaiters == 1 {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("successor request did not join readiness")
}

func sampleLifecycleDiagnostics(ctx context.Context, p *dockerProvider, sampledRetired chan<- struct{}) error {
	var previous workloadServiceCounters
	var sawRetired bool
	for ctx.Err() == nil {
		snapshot := p.snapshotWorkloads()
		service := snapshot.Services[0]
		if service.ActivationAttempts < previous.ActivationAttempts || service.ActivationFailures < previous.ActivationFailures || service.IdleStops < previous.IdleStops {
			return errors.New("concurrent snapshot counters decreased")
		}
		if err := validateDiagnosticOwners(service); err != nil {
			return err
		}
		if len(service.Owners) > 1 && !sawRetired {
			close(sampledRetired)
			sawRetired = true
		}
		previous = service.workloadServiceCounters
		runtime.Gosched()
	}
	return nil
}

func validateDiagnosticOwners(service workloadServiceSnapshot) error {
	incarnations := map[uint64]bool{}
	var current int
	for _, owner := range service.Owners {
		if incarnations[owner.Incarnation] {
			return errors.New("snapshot duplicated incarnation")
		}
		incarnations[owner.Incarnation] = true
		if owner.Current {
			current++
		} else if !owner.Retired {
			return errors.New("detached owner retained authority")
		}
		if owner.ActivationWaiters > 0 && owner.Phase != "starting" {
			return errors.New("snapshot mixed activation waiters with completed phase")
		}
	}
	if current != 1 || service.ActivationFailures > service.ActivationAttempts {
		return errors.New("snapshot mixed owner/counter revisions")
	}
	return nil
}
