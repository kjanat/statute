// Command apidiff compares the exported API surface of the working tree
// against the surface pkg.go.dev published for the module's latest release.
//
// statute's exported API is the product: it is what every importer compiles
// against, and there is no runtime config file to paper over a rename. This
// gate makes a removal or a signature change visible in the pull request that
// introduces it rather than in the issue tracker a week after the tag.
//
// The published side comes from the pkg.go.dev v1 API (unauthenticated, and
// only ever aware of released versions). The local side is parsed straight
// from the working tree with go/doc, then rendered into the same one-line
// synopsis shape pkg.go.dev uses so the two are comparable.
//
// Usage:
//
//	go run ./scripts/apidiff                        compare against pkg.go.dev
//	go run ./scripts/apidiff -save api.json         ... and record the baseline
//	go run ./scripts/apidiff -baseline api.json     compare offline
//
// It exits 1 when a published symbol was removed or changed, 0 when the only
// difference is new API, and 2 when the comparison itself could not be made.
// Set APIDIFF_ALLOW_BREAKING=1 for the one commit that breaks the API on
// purpose: removals and changes are then reported as warnings and the exit
// code stays 0.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
)

// defaultModule is the module this repository publishes. It is a flag default
// rather than a hard-coded constant so the tool can be pointed at a fork.
const defaultModule = "statute.kjanat.dev"

// allowBreakingEnv is the escape hatch for the commit that intentionally
// breaks the API. pkg.go.dev only knows released versions, so an intended
// break would otherwise fail CI until the next tag exists.
const allowBreakingEnv = "APIDIFF_ALLOW_BREAKING"

// Process exit codes. 2 is reserved for "the comparison did not happen" so a
// network failure is never mistaken for a clean surface.
const (
	exitOK       = 0
	exitBreaking = 1
	exitError    = 2
)

func main() {
	code, err := run(os.Args[1:], os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "apidiff: %v\n", err)
		os.Exit(exitError)
	}
	os.Exit(code)
}

// options are the parsed command-line flags plus the environment escape hatch.
type options struct {
	module        string
	dir           string
	baseline      string
	save          string
	allowBreaking bool
}

func parseFlags(args []string) (options, error) {
	var opts options
	fs := flag.NewFlagSet("apidiff", flag.ContinueOnError)
	fs.StringVar(&opts.module, "module", defaultModule, "module path to compare")
	fs.StringVar(&opts.dir, "dir", ".", "module root holding the local surface")
	fs.StringVar(&opts.baseline, "baseline", "", "read the published surface from this file instead of pkg.go.dev")
	fs.StringVar(&opts.save, "save", "", "write the published surface to this file")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	// Any truthy value works; a typo'd value simply leaves the gate armed.
	opts.allowBreaking, _ = strconv.ParseBool(os.Getenv(allowBreakingEnv))
	return opts, nil
}

func run(args []string, out io.Writer) (int, error) {
	opts, err := parseFlags(args)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK, nil
	}
	if err != nil {
		return exitError, err
	}

	local, err := localSurface(opts.dir, opts.module)
	if err != nil {
		return exitError, fmt.Errorf("local surface: %w", err)
	}

	base, err := publishedBaseline(context.Background(), opts)
	if err != nil {
		return exitError, fmt.Errorf("published surface: %w", err)
	}
	if opts.save != "" {
		if err := saveBaseline(opts.save, base); err != nil {
			return exitError, err
		}
	}

	printHeader(out, opts, base, local)
	return printDiff(out, diff(base.Packages, local), opts.allowBreaking), nil
}

// publishedBaseline resolves the baseline from disk when -baseline was given
// and from pkg.go.dev otherwise.
func publishedBaseline(ctx context.Context, opts options) (*Baseline, error) {
	if opts.baseline != "" {
		return loadBaseline(opts.baseline)
	}
	return newClient(pkgSiteAPI).baseline(ctx, opts.module)
}

// say writes one line of the report. The report is what this tool exists to
// produce, but a short write to stdout is not something a comparison can
// recover from or should abort over, so the error is deliberately dropped.
func say(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

// printHeader states what is being compared and how big each side is. The
// per-package counts are the quickest sanity check that the local parse found
// the same packages the module actually publishes.
func printHeader(w io.Writer, opts options, base *Baseline, local Surface) {
	source := "pkg.go.dev"
	if opts.baseline != "" {
		source = opts.baseline
	}
	say(w, "apidiff: %s\n", opts.module)
	say(w, "  published: %s (%s)\n", base.Version, source)
	say(w, "  local:     working tree (%s)\n\n", opts.dir)

	for _, pkg := range packagePaths(base.Packages, local) {
		say(w, "  %-40s %4d published  %4d local\n", pkg, len(base.Packages[pkg]), len(local[pkg]))
	}
	say(w, "\n")
}

// packagePaths is the sorted union of the package paths on both sides, so a
// package that exists on only one of them still shows up in the header.
func packagePaths(surfaces ...Surface) []string {
	seen := map[string]bool{}
	for _, s := range surfaces {
		for pkg := range s {
			seen[pkg] = true
		}
	}
	paths := make([]string, 0, len(seen))
	for pkg := range seen {
		paths = append(paths, pkg)
	}
	slices.Sort(paths)
	return paths
}
