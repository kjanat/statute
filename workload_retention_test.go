package statute

import "testing"

func TestRetiredMutationRetentionDoesNotDependOnRouteEligibility(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		retired bool
		binding *workloadBinding
	}{
		{name: "grant restored", binding: &workloadBinding{key: 1, containerID: "immutable"}},
		{name: "missing binding", retired: true},
		{name: "mismatched binding", retired: true, binding: &workloadBinding{key: 2, containerID: "immutable"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stop := &workloadStop{binding: 1, uncertain: true}
			owner := &workload{phase: workloadStopUnknown, stop: stop, retired: tc.retired, binding: tc.binding}
			settled := &workload{phase: workloadDormant, retired: true}
			provider := &dockerProvider{retiredMutations: []*workload{settled, owner}}

			provider.workloadMu.Lock()
			refs := provider.retiredMutationContainerRefsLocked()
			provider.workloadMu.Unlock()

			if len(refs) != 0 {
				t.Fatalf("ineligible owner produced route references: %v", refs)
			}
			if len(provider.retiredMutations) != 1 || provider.retiredMutations[0] != owner {
				t.Fatal("route ineligibility discarded an unresolved mutation owner")
			}
			if owner.stop != stop || !stop.uncertain {
				t.Fatal("retention changed the unresolved mutation")
			}
		})
	}
}
