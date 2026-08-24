package statutelifecycle

import (
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

func TestAnalyzer(t *testing.T) {
	t.Parallel()
	analysistest.Run(t, testdataDir(t), Analyzer, "a")
}

func testdataDir(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate analyzer testdata")
	}
	return filepath.Join(filepath.Dir(filename), "testdata")
}
