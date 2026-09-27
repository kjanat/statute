package statutelifecycle

import (
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

func TestMutationReviewCounterexamples(t *testing.T) {
	t.Parallel()
	for _, fixture := range []string{
		"review-phase-call", "review-phase-write", "review-storage-write", "review-observation-order", "review-interface-ingress",
	} {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()
			analysistest.Run(t, filepath.Join(testdataDir(t), "docker", fixture), Analyzer, "statute.kjanat.dev")
		})
	}
}
