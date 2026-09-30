//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"statute.kjanat.dev/e2e/harness"
	"statute.kjanat.dev/e2e/report"
)

func TestRegression_EngineContainerLifecycle(t *testing.T) {
	t.Parallel()
	image := os.Getenv("STATUTE_E2E_IMAGE")
	if image == "" {
		t.Fatal("STATUTE_E2E_IMAGE is required")
	}
	project := fmt.Sprintf("statute-e2e-engine-%d", os.Getpid())
	engine := newTestEngine(t, project)
	ctx := t.Context()
	id, err := engine.Create(ctx, harness.ContainerSpec{
		Name: project + "-origin", Image: image, Network: "none", Entrypoint: []string{"/origin"},
	})
	requireEngine(t, err)
	state, err := engine.Inspect(ctx, id)
	requireEngine(t, err)
	if state.Running {
		t.Fatal("created container is already running")
	}
	requireEngine(t, engine.Start(ctx, id))
	state, err = engine.Inspect(ctx, id)
	requireEngine(t, err)
	if !state.Running {
		t.Fatal("started container is not running")
	}
	requireEngine(t, engine.Stop(ctx, id))
	state, err = engine.Inspect(ctx, id)
	requireEngine(t, err)
	if state.Running {
		t.Fatal("stopped container is still running")
	}
	requireEngine(t, engine.Remove(ctx, id))
	requireEngine(t, engine.Cleanup(ctx))
	if _, err := engine.Inspect(ctx, id); err == nil {
		t.Fatal("removed container still exists")
	}
}

func TestRegression_EngineFailedStartCleanup(t *testing.T) {
	t.Parallel()
	image := os.Getenv("STATUTE_E2E_IMAGE")
	if image == "" {
		t.Fatal("STATUTE_E2E_IMAGE is required")
	}
	project := fmt.Sprintf("statute-e2e-start-failure-%d", os.Getpid())
	engine := newTestEngine(t, project)
	ctx := t.Context()
	id, err := engine.Create(ctx, harness.ContainerSpec{
		Name: project + "-invalid", Image: image, Network: "none", Entrypoint: []string{"/missing-e2e-executable"},
	})
	requireEngine(t, err)
	if err := engine.Start(ctx, id); err == nil {
		t.Fatal("invalid entrypoint unexpectedly started")
	}
	requireEngine(t, engine.Cleanup(ctx))
	if _, err := engine.Inspect(ctx, id); err == nil {
		t.Fatal("failed start escaped owned cleanup")
	}
}

func TestRegression_ClientObservationBatch(t *testing.T) {
	t.Parallel()
	r := harness.StartServices(t, "mesh", harness.MustTopology(t, "1s1c"), []string{"origin-1"})
	ctx := t.Context()
	target := fmt.Sprintf("http://origin-1:%d/fail?key=batch&n=2", harness.PortOrigin)
	body := awaitClient(ctx, r, target, 15*time.Second, report.WaitSpec{
		Contains: []string{`"origin":"origin-1"`}, Consecutive: 2,
	})
	if !strings.Contains(body, `"origin":"origin-1"`) {
		t.Fatalf("matching snapshot missing: %s", body)
	}
	count := 0
	for _, entry := range originJournal(ctx, r, "origin-1", "http") {
		if entry.Query == "key=batch&n=2" {
			count++
		}
	}
	if count != 4 {
		t.Fatalf("one actor made %d observations, want two failures followed by two matches", count)
	}
}
