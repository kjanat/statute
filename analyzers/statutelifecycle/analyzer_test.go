package statutelifecycle

import (
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

func TestAnalyzer(t *testing.T) {
	t.Parallel()
	analysistest.Run(t, filepath.Join(testdataDir(t), "core"), Analyzer, "a")
}

func TestDockerMutationAnalyzer(t *testing.T) {
	t.Parallel()
	for _, fixture := range []string{
		"valid",
		"raw-call",
		"boundary-context",
		"missing-cancellation",
		"asynchronous-call",
		"persistence",
		"settlement-boundaries",
		"settlement-delete-order",
		"settlement-registry-provenance",
		"settlement-owner-revalidation",
		"settlement-ownership-order",
		"settlement-generation-fencing",
	} {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()
			analysistest.Run(t, filepath.Join(testdataDir(t), "docker", fixture), Analyzer, "statute.kjanat.dev")
		})
	}
}

func testdataDir(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate analyzer testdata")
	}
	return filepath.Join(filepath.Dir(filename), "testdata")
}
