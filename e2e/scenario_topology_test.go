//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"statute.kjanat.dev/e2e/harness"
)

// TestRegression_DockerScenarioTopology validates the startup precondition
// directly, without creating artifacts, containers or another lifecycle owner.
func TestRegression_DockerScenarioTopology(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"docker", "workload", "workload-concurrency"} {
		for _, topology := range harness.Topologies {
			t.Run(scenario+"/"+topology.Name, func(t *testing.T) {
				err := harness.ValidateScenarioTopology(scenario, topology, topology.Servers)
				wantError := len(topology.Servers) != 1
				if (err != nil) != wantError {
					t.Fatalf("validation error=%v, wantError=%v", err, wantError)
				}
			})
		}
	}
}

func TestRegression_DockerScenarioRejectsMalformedOwnership(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"docker", "workload", "workload-concurrency"} {
		for _, tc := range []struct {
			name, topologyName string
			servers, services  []string
		}{
			{"empty", "1s1c", nil, nil},
			{"replacement", "1s1c", []string{harness.Server2}, nil},
			{"unexpected owner", "1s1c", []string{"another-server"}, nil},
			{"duplicate owner", "1s1c", []string{harness.Server1, harness.Server1}, nil},
			{"explicit second server", "1s1c", []string{harness.Server1}, []string{harness.Server1, harness.Server2}},
			{"two-server filename with one declared server", "2s1c", []string{harness.Server1}, nil},
			{"unknown filename", "unknown", []string{harness.Server1}, nil},
		} {
			t.Run(scenario+"/"+tc.name, func(t *testing.T) {
				topology := harness.Topology{Name: tc.topologyName, Servers: tc.servers, Clients: []string{harness.Client1}}
				err := harness.ValidateScenarioTopology(scenario, topology, tc.services)
				if err == nil || !strings.Contains(err.Error(), scenario) {
					t.Fatalf("unsupported ownership did not produce a scenario-specific error: %v", err)
				}
			})
		}
	}
}

func TestRegression_MeshScenarioTopologyUnchanged(t *testing.T) {
	t.Parallel()
	for _, topology := range harness.Topologies {
		t.Run(topology.Name, func(t *testing.T) {
			if err := harness.ValidateScenarioTopology("mesh", topology, []string{harness.Server1, harness.Server2}); err != nil {
				t.Fatalf("mesh topology rejected: %v", err)
			}
		})
	}
}
