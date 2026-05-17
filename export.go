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

	if countTrue(export, validate, graph, lint) > 1 {
		fmt.Fprintln(os.Stderr, "statute: -export, -validate, -graph, and -lint are mutually exclusive")
		os.Exit(2)
	}

	switch {
	case *export:
		exitOnErr(Export(cfg, os.Stdout))
	case *validate:
		_, err := Resolve(cfg)
		exitOnErr(err)
		fmt.Println("ok")
	case *graph:
		exitOnErr(GraphDOT(cfg, os.Stdout))
	case *lint:
		runLint(cfg)
	default:
		Run(cfg)
	}
}

func countTrue(flags ...*bool) int {
	count := 0
	for _, b := range flags {
		if *b {
			count++
		}
	}
	return count
}

// exitOnErr prints err to stderr and exits 1 when err is non-nil.
func exitOnErr(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "statute:", err)
		os.Exit(1)
	}
}

// runLint audits the configuration and exits 1 if any error-severity finding
// fires. Findings are always printed first so warnings remain visible.
func runLint(cfg Config) {
	findings, err := Lint(cfg)
	exitOnErr(err)
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
}
