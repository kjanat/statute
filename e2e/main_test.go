//go:build e2e

// Package e2e holds the black-box scenario tests of the lane. Tier
// selection is by name: TestSmoke_* is the PR gate, TestRegression_*
// the deterministic regression tier, TestSoak_* the scheduled
// stress/soak tier. The Makefile targets pass the matching -run
// expressions.
package e2e

import (
	"context"
	"fmt"
	"os"
	"testing"

	"statute.kjanat.dev/e2e/harness"
)

// TestMain runs the suite and then the lane-wide orphan epilogue: after
// everything finished, no statute.e2e-labeled container may remain on
// the host, no matter which run would have leaked it. A reaped orphan
// fails the whole invocation even when every test passed.
func TestMain(m *testing.M) {
	code := m.Run()
	if leaked := harness.SweepLaneOrphans(context.Background()); len(leaked) > 0 {
		fmt.Fprintf(os.Stderr, "e2e: lane orphan containers reaped after the suite: %v\n", leaked)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
