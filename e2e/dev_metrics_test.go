//go:build e2e

package e2e

import (
	"encoding/json"
	"testing"

	"statute.kjanat.dev/e2e/harness"
)

// TestRegression_DevMetricsPublication checks the actual example's rendered
// port mappings, including any additional publication of the metrics port.
func TestRegression_DevMetricsPublication(t *testing.T) {
	const project = "statute-e2e-dev-metrics-render"
	compose := harness.Compose{
		Project: project, Dir: "../examples/dev", Files: []string{"docker-compose.yml"},
		Engine: newTestEngine(t, project),
	}
	output, err := compose.Output(t.Context(), "config", "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	var rendered struct {
		Services map[string]struct {
			Ports []struct {
				Target    int    `json:"target"`
				Published string `json:"published"`
				HostIP    string `json:"host_ip"`
				Protocol  string `json:"protocol"`
			} `json:"ports"`
		} `json:"services"`
	}
	if err := json.Unmarshal([]byte(output), &rendered); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, port := range rendered.Services["proxy"].Ports {
		if port.Target != 9090 {
			continue
		}
		found = true
		if port.HostIP != "127.0.0.1" || port.Published != "9090" || port.Protocol != "tcp" {
			t.Errorf("metrics must publish only on IPv4 loopback: %+v", port)
		}
	}
	if !found {
		t.Fatal("development proxy has no metrics publication")
	}
}
