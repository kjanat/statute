//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
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
		{"empty", []string{"-e", "STATUTE_DISCOVERY_SERVICE="}},
		{"whitespace", []string{"-e", "STATUTE_DISCOVERY_SERVICE= \t"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := fmt.Sprintf("statute-e2e-prerequisite-%d-%s", os.Getpid(), tc.name)
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				out, err := exec.CommandContext(ctx, "docker", "rm", "-f", name).CombinedOutput()
				if err != nil && !strings.Contains(string(out), "No such container") {
					t.Errorf("remove prerequisite container: %v\n%s", err, out)
				}
			})
			args := make([]string, 0, 13+len(tc.env))
			args = append(args, "run", "--rm", "--name", name, "--network", "none",
				"--label", "statute.e2e=1", "--entrypoint", "/statute", "-e", "STATUTE_SCENARIO=docker")
			args = append(args, tc.env...)
			args = append(args, image)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 2 {
				t.Fatalf("invalid prerequisite must exit 2: %v\n%s", err, out)
			}
			const diagnostic = "statute-e2e: STATUTE_DISCOVERY_SERVICE is required for the docker scenario"
			if strings.TrimSpace(string(out)) != diagnostic {
				t.Fatalf("prerequisite diagnostic = %q, want %q", out, diagnostic)
			}
		})
	}
}
