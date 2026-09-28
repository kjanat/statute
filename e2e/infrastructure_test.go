//go:build e2e

package e2e

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"statute.kjanat.dev/e2e/harness"
)

// TestRegression_InfrastructureDiscoveryIsolation renders every checked-in
// Compose combination. Cleanup labels must never grant Docker routing intent.
func TestRegression_InfrastructureDiscoveryIsolation(t *testing.T) {
	topologies := infrastructureComposeFiles(t, "topologies/*.yml")
	scenarios := append([]string{""}, infrastructureComposeFiles(t, "scenarios/*/compose.yml")...)
	for _, topology := range topologies {
		for _, scenario := range scenarios {
			name := strings.TrimSuffix(filepath.Base(topology), ".yml") + "/base"
			if scenario != "" {
				name = strings.TrimSuffix(filepath.Base(topology), ".yml") + "/" + filepath.Base(filepath.Dir(scenario))
			}
			t.Run(name, func(t *testing.T) {
				assertInfrastructureCompose(t, topology, scenario)
			})
		}
	}
}

func infrastructureComposeFiles(t *testing.T, pattern string) []string {
	t.Helper()
	paths, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("no Compose files matched %q", pattern)
	}
	return paths
}

type infrastructureService struct {
	Labels      map[string]string `json:"labels"`
	Environment map[string]string `json:"environment"`
	Networks    map[string]struct {
		Aliases []string `json:"aliases"`
	} `json:"networks"`
}

func assertInfrastructureCompose(t *testing.T, topology, scenario string) {
	t.Helper()
	const project = "statute-e2e-infrastructure-render"
	files := []string{"compose.yml", topology}
	if scenario != "" {
		files = append(files, scenario)
	}
	compose := harness.Compose{
		Project: project, Files: files, Dir: ".",
		Env: map[string]string{
			"STATUTE_E2E_IMAGE": "statute-e2e:compose-check",
			"STATUTE_SCENARIO":  "compose-isolation",
			"E2E_REPORTS":       t.TempDir(),
		},
	}
	output, err := compose.Output(t.Context(), "config", "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	var rendered struct {
		Services map[string]infrastructureService `json:"services"`
	}
	if err := json.Unmarshal([]byte(output), &rendered); err != nil {
		t.Fatal(err)
	}
	if len(rendered.Services) == 0 {
		t.Fatal("rendered Compose stack has no services")
	}
	for name, service := range rendered.Services {
		if service.Labels["statute.e2e"] != "1" || service.Labels["statute.enable"] != "false" {
			t.Errorf("infrastructure service %s lacks cleanup label or explicit discovery opt-out: %v", name, service.Labels)
		}
	}
	assertInfrastructureIdentity(t, rendered.Services["statute-1"], project, scenario)
}

func assertInfrastructureIdentity(t *testing.T, node infrastructureService, project, scenario string) {
	t.Helper()
	aliases := node.Networks["mesh"].Aliases
	if !slices.Contains(aliases, project+".test") {
		t.Errorf("project-scoped node alias lost after Compose merge: %v", aliases)
	}
	switch scenario {
	case filepath.Join("scenarios", "acme-http01", "compose.yml"):
		if !slices.Contains(aliases, "proxy.e2e.test") {
			t.Errorf("ACME challenge alias lost after Compose merge: %v", aliases)
		}
	case filepath.Join("scenarios", "docker", "compose.yml"):
		if node.Environment["STATUTE_WORKLOAD_SERVICE"] != project+"-wl" || node.Environment["STATUTE_DISCOVERY_SERVICE"] != project+"-dyn" {
			t.Errorf("Docker service identities are not project-scoped: %v", node.Environment)
		}
	}
}
