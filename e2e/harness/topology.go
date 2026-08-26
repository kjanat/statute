//go:build e2e

// Package harness is the host-side orchestrator of the e2e lane. It owns
// the Compose project, per-run naming, readiness, artifact collection,
// teardown ordering, and the orphan proof. It observes Statute only over
// the network and through artifacts — never in process — and the lint
// import guard makes that boundary mechanical.
package harness

import "fmt"

// Stable node identities of the base stack and its overrides.
const (
	Server1 = "statute-1"
	Server2 = "statute-2"
	Client1 = "client-1"
	Client2 = "client-2"
)

// Topology is one of the four server/client combinations the lane must
// exercise.
type Topology struct {
	// Name is the stable identifier used in override filenames and
	// artifact paths: "1s1c", "1s2c", "2s1c", "2s2c".
	Name    string
	Servers []string
	Clients []string
}

// Topologies is the complete matrix in execution order. Node identities
// are stable service names; nothing relies on DNS round-robin replicas,
// so every client-to-server edge can be asserted individually.
var Topologies = []Topology{
	{Name: "1s1c", Servers: []string{Server1}, Clients: []string{Client1}},
	{Name: "1s2c", Servers: []string{Server1}, Clients: []string{Client1, Client2}},
	{Name: "2s1c", Servers: []string{Server1, Server2}, Clients: []string{Client1}},
	{Name: "2s2c", Servers: []string{Server1, Server2}, Clients: []string{Client1, Client2}},
}

// TopologyByName resolves one matrix entry.
func TopologyByName(name string) (Topology, error) {
	for _, t := range Topologies {
		if t.Name == name {
			return t, nil
		}
	}
	return Topology{}, fmt.Errorf("unknown topology %q", name)
}
