//go:build e2e

// The Statute binary under test. Statute is config-as-code — the binary
// IS the configuration — so the lane's "one image per commit" contract
// is met by compiling every scenario's config into this one entrypoint
// and selecting the active one with STATUTE_SCENARIO. Only this package
// (and no other e2e package) may import statute; the depguard rule in
// .golangci.yml enforces that.
package main

import (
	"fmt"
	"os"
	"sort"

	statute "statute.kjanat.dev"
)

// scenarios maps STATUTE_SCENARIO values to config builders. The node
// argument ("1" or "2") is the Statute instance identity within the
// topology; scenarios whose nodes differ use it, symmetric ones ignore
// it.
var scenarios = map[string]func(node string) statute.Config{
	"mesh": meshConfig,
}

func main() {
	name := os.Getenv("STATUTE_SCENARIO")
	build, ok := scenarios[name]
	if !ok {
		known := make([]string, 0, len(scenarios))
		for k := range scenarios {
			known = append(known, k)
		}
		sort.Strings(known)
		fmt.Fprintf(os.Stderr, "statute-e2e: unknown STATUTE_SCENARIO %q (known: %v)\n", name, known)
		os.Exit(2)
	}
	node := os.Getenv("STATUTE_NODE")
	if node == "" {
		node = "1"
	}
	statute.Run(build(node))
}
