// Command statute provides offline configuration migration tooling.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/alecthomas/kong"

	"statute.kjanat.dev/internal/migrate"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

type cli struct {
	Migrate struct {
		Traefik traefikCommand `cmd:"" help:"Audit Traefik configuration and generate a Statute Go skeleton."`
	} `cmd:"" help:"Audit and migrate configuration from another proxy."`
}

type traefikCommand struct {
	PathMap []string `help:"Map a source provider path to a supplied local path (repeatable)."      placeholder:"SOURCE=LOCAL" sep:"none"`
	Static  []string `help:"Traefik static YAML/TOML file (repeatable)."                            placeholder:"FILE"         sep:"none"`
	Dynamic []string `help:"File-provider YAML/TOML file or directory (repeatable)."                placeholder:"FILE_OR_DIR"  sep:"none"`
	Compose []string `help:"Compose file containing Traefik labels (repeatable; no interpolation)." placeholder:"FILE"         sep:"none"`
	Out     string   `help:"Write Go source to a new file; never overwrite an existing file."       placeholder:"FILE"         required:""`
	Report  string   `help:"Write the JSON audit to a new file; never overwrite an existing file."  placeholder:"FILE"         required:""`
}

type commandOutput struct{ stdout io.Writer }

var errMigrationBlocked = errors.New("migration has blocking findings")

func run(args []string, stdout, stderr io.Writer) int {
	var command cli
	exitCode := -1
	parser, err := kong.New(&command,
		kong.Name("statute"),
		kong.Description("Audit and migrate proxy configuration to Statute."),
		kong.Writers(stdout, stderr),
		kong.ConfigureHelp(kong.HelpOptions{NoExpandSubcommands: true}),
		kong.Exit(func(code int) { exitCode = code }),
	)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "cannot initialize command-line parser:", err)
		return 2
	}
	if len(args) == 0 {
		args = []string{"--help"}
	}
	ctx, err := parser.Parse(args)
	if exitCode >= 0 {
		return exitCode
	}
	if err != nil {
		parser.Errorf("%s", err)
		_, _ = fmt.Fprintln(stderr, `Run "statute --help" for usage.`)
		return 2
	}
	if err := ctx.Run(&commandOutput{stdout: stdout}); err != nil {
		if errors.Is(err, errMigrationBlocked) {
			return 1
		}
		parser.Errorf("%s", err)
		return 2
	}
	return 0
}

func (c *traefikCommand) Validate() error {
	if len(c.Static)+len(c.Dynamic)+len(c.Compose) == 0 {
		return errors.New("provide at least one of --static, --dynamic or --compose")
	}
	for _, mapping := range c.PathMap {
		from, to, ok := strings.Cut(mapping, "=")
		if !ok || !filepath.IsAbs(from) || to == "" {
			return errors.New("--path-map requires an absolute SOURCE and nonempty LOCAL path")
		}
	}
	if samePath(c.Out, c.Report) {
		return errors.New("output paths must be distinct")
	}
	return nil
}

func (*traefikCommand) Help() string {
	return `Example:
  statute migrate traefik --static traefik.yml --dynamic dynamic/ --compose compose.yml --out main.go --report migration.json

Runs offline. Review the generated source and audit before deploying.
Exit status: 0 = no migration gaps, 1 = blocking findings, 2 = invocation or I/O error.`
}

func (c *traefikCommand) Run(output *commandOutput) error {
	outputs, err := reserveOutputs(c)
	if err != nil {
		return err
	}
	defer outputs.cleanup()
	mappings := map[string]string{}
	for _, mapping := range c.PathMap {
		from, to, _ := strings.Cut(mapping, "=")
		mappings[filepath.Clean(from)] = to
	}
	result, err := migrate.Audit(migrate.Inputs{Static: c.Static, Dynamic: c.Dynamic, Compose: c.Compose, PathMappings: mappings})
	if err != nil {
		return fmt.Errorf("migration: %w", err)
	}
	if err = outputs.write(result); err != nil {
		return errors.New("cannot write migration artifacts")
	}
	if err = result.Report.WriteHuman(output.stdout); err != nil {
		return errors.New("cannot write audit summary")
	}
	if result.Report.Blocking {
		return errMigrationBlocked
	}
	return nil
}

type artifacts struct {
	source, report *os.File
	complete       bool
}

func reserveOutputs(command *traefikCommand) (*artifacts, error) {
	source, err := createOutput(command.Out)
	if err != nil {
		return nil, errors.New("cannot create Go output (must be a new file)")
	}
	report, err := createOutput(command.Report)
	if err != nil {
		_ = source.Close()
		_ = os.Remove(command.Out)
		return nil, errors.New("cannot create report output (must be a new file)")
	}
	return &artifacts{source: source, report: report}, nil
}

func (a *artifacts) cleanup() {
	_ = a.source.Close()
	_ = a.report.Close()
	if !a.complete {
		// #nosec G703 -- This invocation exclusively created the operator-selected file.
		_ = os.Remove(a.source.Name())
		// #nosec G703 -- This invocation exclusively created the operator-selected file.
		_ = os.Remove(a.report.Name())
	}
}

func (a *artifacts) write(r *migrate.Result) error {
	enc := json.NewEncoder(a.report)
	enc.SetIndent("", "  ")
	if _, err := a.source.Write(r.Go); err != nil {
		return err
	}
	if err := enc.Encode(r.Report); err != nil {
		return err
	}
	if err := errors.Join(a.source.Close(), a.report.Close()); err != nil {
		return err
	}
	a.complete = true
	return nil
}

func samePath(a, b string) bool {
	x, ex := filepath.Abs(a)
	y, ey := filepath.Abs(b)
	return ex == nil && ey == nil && x == y
}

func createOutput(path string) (*os.File, error) {
	if path == "" {
		return nil, errors.New("empty path")
	}
	// #nosec G304 -- Explicit operator output, exclusive creation prevents replacement.
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}
