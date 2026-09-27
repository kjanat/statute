package statute

import (
	"context"
	"testing"

	"statute.kjanat.dev/resolved"
)

func TestPersistedStopFallbackKeepsPredecessorAfterBindingReplacement(t *testing.T) {
	t.Parallel()
	config := &resolved.Docker{}
	provider, _, daemon := newFakeProviderDaemon(t, config, []fakeDaemonContainer{
		{id: "predecessor-id", name: "shared-name"},
	})
	registry, err := openMutationRegistry(config.Storage, config.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	provider.lifecycleMu.Lock()
	provider.current = &dockerRun{provider: provider, registry: registry}
	provider.lifecycleMu.Unlock()

	stop := &workloadStop{kind: workloadIdleStop, binding: 1, ref: "shared-name"}
	owner := &workload{
		service: "wl", phase: workloadStopIssued, stop: stop,
		binding: &workloadBinding{key: 1, container: "shared-name", containerID: "predecessor-id"},
	}
	if err := provider.persistOwnedStop(owner, stop); err != nil {
		t.Fatalf("persist predecessor stop: %v", err)
	}
	if stop.ref != "predecessor-id" || !stop.persisted || !registry.contains("predecessor-id") {
		t.Fatalf("persisted target = %q, persisted=%v; want immutable predecessor", stop.ref, stop.persisted)
	}

	daemon.swap([]fakeDaemonContainer{
		{id: "predecessor-id", name: "retired-predecessor"},
		{id: "successor-id", name: "shared-name"},
	})
	owner.mu.Lock()
	owner.binding = &workloadBinding{key: 2, container: "shared-name", containerID: "successor-id"}
	owner.mu.Unlock()

	attempt := provider.attemptOwnedStop(context.Background(), owner, stop)
	if attempt.result != workloadStopSucceeded {
		t.Fatalf("predecessor stop result = %v, want success", attempt.result)
	}
	if daemon.stopCount("retired-predecessor") != 1 || daemon.stopCount("shared-name") != 0 {
		t.Fatal("stale issued stop followed the reused name to its successor")
	}
}
