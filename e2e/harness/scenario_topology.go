//go:build e2e

package harness

import (
	"fmt"
	"slices"
)

// ValidateScenarioTopology rejects unsupported Docker scenario ownership before
// startup creates artifacts or invokes Compose. Other scenarios retain their
// existing topology and explicit-service behavior.
func ValidateScenarioTopology(scenario string, topology Topology, services []string) error {
	switch scenario {
	case "docker", "workload", "workload-concurrency":
		canonical, err := TopologyByName(topology.Name)
		if err != nil {
			return fmt.Errorf("scenario %q: %w", scenario, err)
		}
		if !slices.Equal(canonical.Servers, []string{Server1}) || !slices.Equal(topology.Servers, []string{Server1}) {
			return fmt.Errorf("scenario %q requires a one-server topology and Servers exactly [%s]; multi-server Docker scenarios are unsupported", scenario, Server1)
		}
		if slices.Contains(services, Server2) {
			return fmt.Errorf("scenario %q cannot start %s; Docker scenarios support only %s", scenario, Server2, Server1)
		}
	}
	return nil
}
