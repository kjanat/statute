package statutelifecycle

import (
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

func TestDockerMutationHelperAnalyzer(t *testing.T) {
	t.Parallel()
	for _, fixture := range []string{
		"reference-valid", "reference-invalid", "persistence-helper-valid", "persistence-helper-invalid",
		"persistence-helper-early-flag", "persistence-helper-stale-owner", "persistence-helper-mutated-record",
		"attempt-classification-valid", "attempt-classification-invalid",
		"owner-tuple-valid", "owner-tuple-invalid",
		"attempt-classification-repeat", "attempt-classification-target", "owner-tuple-mutated",
		"terminal-replay-valid", "terminal-replay-invalid",
		"identity-valid", "identity-invalid",
		"binding-state-valid", "binding-state-invalid",
		"allocator-valid", "allocator-invalid", "allocator-stale",
		"persistence-fallback-invalid",
		"persistence-fallback-range",
	} {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()
			analysistest.Run(t, filepath.Join(testdataDir(t), "docker", fixture), Analyzer, "statute.kjanat.dev")
		})
	}
}
