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

// Main is a thin CLI wrapper around Run, Export, and a validate-only mode. It
// parses the standard process arguments and dispatches:
//
//	-export    Write the resolved configuration as JSON to stdout and exit.
//	-validate  Validate the configuration and exit. Prints "ok" on success.
//	(no flag)  Equivalent to Run.
//
// Programs that want a clean entry point without flag handling can call Run
// directly.
func Main(cfg Config) {
	export := flag.Bool("export", false, "write resolved configuration as JSON to stdout and exit")
	validate := flag.Bool("validate", false, "validate configuration and exit")
	flag.Parse()

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
	default:
		Run(cfg)
	}
}
