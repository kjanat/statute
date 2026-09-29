//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"statute.kjanat.dev/e2e/harness"
)

func mustCreateContainer(ctx context.Context, r *harness.Run, spec harness.ContainerSpec, start bool) string {
	r.T.Helper()
	id, err := r.Engine.Create(ctx, spec)
	if err != nil {
		r.T.Fatal(err)
	}
	if start {
		requireEngine(r.T, r.Engine.Start(ctx, id))
	}
	return id
}

func requireEngine(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func newTestEngine(t *testing.T, project string) *harness.Engine {
	t.Helper()
	engine, err := harness.NewEngine(t.Context(), project)
	requireEngine(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := engine.Cleanup(ctx); err != nil {
			t.Error(err)
		}
		if err := engine.Close(); err != nil {
			t.Error(err)
		}
	})
	return engine
}
