package statute

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
)

// Export validates and resolves the surface configuration, then writes the
// canonical resolved schema as JSON. Useful for diffing deployments and
// snapshotting in CI without starting a server.
func Export(cfg Config, w io.Writer) error {
	resolved, err := Resolve(cfg)
	if err != nil {
		return fmt.Errorf("resolve: %w", err)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(resolved)
}

// Main is a thin CLI wrapper around Run, Export, Lint, and Graph. It
// parses the standard process arguments and dispatches:
//
//	-export    Write the resolved configuration as JSON to stdout and exit.
//	-validate  Validate the configuration and exit. Prints "ok" on success.
//	-graph     Write the resolved topology as Graphviz DOT to stdout and exit.
//	-lint      Audit the resolved configuration against the production-readiness
//	           rule set; exit non-zero if any error-severity finding fires.
//	(no flag)  Equivalent to Run.
//
// The four operation flags are mutually exclusive. Programs that want a
// clean entry point without flag handling can call Run directly.
func Main(cfg Config) {
	export := flag.Bool("export", false, "write resolved configuration as JSON to stdout and exit")
	validate := flag.Bool("validate", false, "validate configuration and exit")
	graph := flag.Bool("graph", false, "write topology as Graphviz DOT to stdout and exit")
	lint := flag.Bool("lint", false, "audit configuration against the production checklist")
	flag.Parse()

	// Enforce mutual exclusion.
	count := 0
	for _, b := range []*bool{export, validate, graph, lint} {
		if *b {
			count++
		}
	}
	if count > 1 {
		fmt.Fprintln(os.Stderr, "statute: -export, -validate, -graph, and -lint are mutually exclusive")
		os.Exit(2)
	}

	switch {
	case *export:
		if err := Export(cfg, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "statute:", err)
			os.Exit(1)
		}
	case *validate:
		if _, err := Resolve(cfg); err != nil {
			fmt.Fprintln(os.Stderr, "statute:", err)
			os.Exit(1)
		}
		fmt.Println("ok")
	case *graph:
		if err := GraphDOT(cfg, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "statute:", err)
			os.Exit(1)
		}
	case *lint:
		findings, err := Lint(cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "statute:", err)
			os.Exit(1)
		}
		hasError := false
		for _, f := range findings {
			fmt.Println(f.String())
			if f.Severity == SeverityError {
				hasError = true
			}
		}
		if hasError {
			os.Exit(1)
		}
	default:
		Run(cfg)
	}
}
