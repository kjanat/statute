//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"statute.kjanat.dev/e2e/harness"
)

func TestRegression_DockerDiscoveryRequiresService(t *testing.T) {
	image := os.Getenv("STATUTE_E2E_IMAGE")
	if image == "" {
		t.Fatal("STATUTE_E2E_IMAGE is required; run make test-e2e-regression")
	}
	for _, tc := range []struct {
		name string
		env  []string
	}{
		{"unset", nil},
		{"empty", []string{"STATUTE_DISCOVERY_SERVICE="}},
		{"whitespace", []string{"STATUTE_DISCOVERY_SERVICE= \t"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := fmt.Sprintf("statute-e2e-prerequisite-%d-%s", os.Getpid(), tc.name)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			engine := newTestEngine(t, name)
			id, err := engine.Create(ctx, harness.ContainerSpec{
				Name: name, Image: image, Network: "none", Entrypoint: []string{"/statute"},
				Env: append([]string{"STATUTE_SCENARIO=docker"}, tc.env...),
			})
			requireEngine(t, err)
			requireEngine(t, engine.Start(ctx, id))
			code, err := engine.Wait(ctx, id)
			requireEngine(t, err)
			out, err := engine.Logs(ctx, id)
			requireEngine(t, err)
			if code != 2 {
				t.Fatalf("invalid prerequisite exit = %d, want 2\n%s", code, out)
			}
			const diagnostic = "statute-e2e: STATUTE_DISCOVERY_SERVICE is required for the docker scenario"
			if strings.TrimSpace(out) != diagnostic {
				t.Fatalf("prerequisite diagnostic = %q, want %q", out, diagnostic)
			}
		})
	}
}
