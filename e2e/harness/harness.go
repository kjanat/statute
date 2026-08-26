//go:build e2e

package harness

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"statute.kjanat.dev/e2e/report"
)

// Conventional in-network ports every scenario config uses, so the
// harness and the client plans can address any Statute node uniformly.
const (
	// PortHTTP is the plain-HTTP content listener.
	PortHTTP = 8080
	// PortHTTPS is the TLS content listener where a scenario declares one.
	PortHTTPS = 8443
	// PortHealth is the statute.Health endpoint.
	PortHealth = 8081
	// PortMetrics is the Prometheus endpoint where a scenario declares one.
	PortMetrics = 9090
	// PortOrigin is every origin actor's listen port.
	PortOrigin = 7000
)

// Run is one scenario execution on one topology: a unique Compose
// project plus its artifact directory and teardown obligations.
type Run struct {
	T        *testing.T
	Scenario string
	Topology Topology
	Compose  *Compose

	// ArtifactDir holds everything this run leaves behind; ReportsDir is
	// the subdirectory bind-mounted into every container at /reports.
	ArtifactDir string
	ReportsDir  string
}

// Start brings one scenario up on one topology. The image reference
// must arrive via STATUTE_E2E_IMAGE (the Makefile builds it once per
// commit); a missing prerequisite fails the test — the lane never
// silently skips declared coverage.
func Start(t *testing.T, scenario string, topo Topology, extraFiles ...string) *Run {
	t.Helper()
	return StartServices(t, scenario, topo, append(append([]string{}, topo.Servers...), origins()...), extraFiles...)
}

// StartServices is Start with an explicit set of services to bring up —
// for scenarios that add supporting services (Pebble, a collector) or
// must observe a Statute node failing to start rather than waiting for
// it to run.
func StartServices(t *testing.T, scenario string, topo Topology, services []string, extraFiles ...string) *Run {
	t.Helper()
	image := os.Getenv("STATUTE_E2E_IMAGE")
	if image == "" {
		t.Fatal("STATUTE_E2E_IMAGE is not set; run through `make test-e2e` or build the image and export the variable")
	}
	runID := randomID()
	artifactDir, err := filepath.Abs(filepath.Join("artifacts", runID, scenario, topo.Name))
	if err != nil {
		t.Fatal(err)
	}
	reportsDir := filepath.Join(artifactDir, "reports")
	if err := os.MkdirAll(reportsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The reports mount is written by container users that do not match
	// the host uid (the collector's file exporter, non-root actors).
	if err := os.Chmod(reportsDir, 0o777); err != nil {
		t.Fatal(err)
	}
	files := append([]string{"compose.yml", filepath.Join("topologies", topo.Name+".yml")}, extraFiles...)
	r := &Run{
		T:        t,
		Scenario: scenario,
		Topology: topo,
		Compose: &Compose{
			Project: fmt.Sprintf("statute-e2e-%s-%s-%s", sanitize(scenario), topo.Name, runID),
			Files:   files,
			Env: map[string]string{
				"STATUTE_E2E_IMAGE": image,
				"STATUTE_SCENARIO":  scenario,
				"E2E_REPORTS":       reportsDir,
			},
		},
		ArtifactDir: artifactDir,
		ReportsDir:  reportsDir,
	}
	// Teardown runs on success, failure, and test timeout alike;
	// diagnostics come first because down removes their sources.
	t.Cleanup(r.teardown)

	if len(services) > 0 {
		if err := r.Compose.Up(context.Background(), services...); err != nil {
			t.Fatalf("compose up: %v", err)
		}
	}
	return r
}

// origins returns the origin services the base stack always provides.
func origins() []string {
	return []string{"origin-1", "origin-2"}
}

// AwaitReady proves every Statute node in the topology is serving by
// polling its health endpoint's ready path from inside the network. A
// running container, a bound socket, or a started process is not
// readiness; only the 200 the runtime commits after Start is.
func (r *Run) AwaitReady(ctx context.Context) {
	r.T.Helper()
	for _, server := range r.Topology.Servers {
		url := fmt.Sprintf("http://%s:%d/healthz/ready", server, PortHealth)
		out, err := r.Compose.RunClient(ctx, r.Topology.Clients[0], "wait", "-url", url, "-timeout", "60s")
		if err != nil {
			r.T.Fatalf("readiness of %s: %v\n%s", server, err, out)
		}
	}
}

// WritePlan places one client's plan on the shared mount and returns
// its in-container path.
func (r *Run) WritePlan(client string, plan *report.Plan) string {
	r.T.Helper()
	path, err := r.WritePlanE(client, plan)
	if err != nil {
		r.T.Fatal(err)
	}
	return path
}

// WritePlanE is WritePlan without a fatal failure, for callers off the
// test goroutine.
func (r *Run) WritePlanE(client string, plan *report.Plan) (string, error) {
	plan.ClientID = client
	plan.Topology = r.Topology.Name
	name := fmt.Sprintf("%s-%s-plan.json", client, sanitize(plan.Name))
	blob, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return "", fmt.Errorf("plan %s: %w", plan.Name, err)
	}
	if err := os.WriteFile(filepath.Join(r.ReportsDir, name), blob, 0o644); err != nil {
		return "", fmt.Errorf("plan %s: %w", plan.Name, err)
	}
	return "/reports/" + name, nil
}

// ExecutePlan runs one client's plan to completion and parses the
// report it wrote back. The client process exits non-zero when any
// planned edge had no successful traversal, which fails the test here.
func (r *Run) ExecutePlan(ctx context.Context, client string, plan *report.Plan) *report.Report {
	r.T.Helper()
	rep, err := r.ExecutePlanE(ctx, client, plan)
	if err != nil {
		r.T.Fatal(err)
	}
	return rep
}

// ExecutePlanE is ExecutePlan for callers off the test goroutine: a
// fatal failure there would abandon the caller's synchronization instead
// of failing the test, so the whole path returns errors.
func (r *Run) ExecutePlanE(ctx context.Context, client string, plan *report.Plan) (*report.Report, error) {
	planPath, err := r.WritePlanE(client, plan)
	if err != nil {
		return nil, err
	}
	outName := fmt.Sprintf("%s-%s-report.json", client, sanitize(plan.Name))
	out, err := r.Compose.RunClient(ctx, client, "run", "-plan", planPath, "-out", "/reports/"+outName)
	if err != nil {
		return nil, fmt.Errorf("client %s plan %s: %w\n%s", client, plan.Name, err, out)
	}
	return r.ReadReportE(outName)
}

// ReadReport parses one report file from the shared mount.
func (r *Run) ReadReport(name string) *report.Report {
	r.T.Helper()
	rep, err := r.ReadReportE(name)
	if err != nil {
		r.T.Fatal(err)
	}
	return rep
}

// ReadReportE is ReadReport without a fatal failure, for callers off the
// test goroutine.
func (r *Run) ReadReportE(name string) (*report.Report, error) {
	blob, err := os.ReadFile(filepath.Join(r.ReportsDir, name))
	if err != nil {
		return nil, fmt.Errorf("report %s: %w", name, err)
	}
	var rep report.Report
	if err := json.Unmarshal(blob, &rep); err != nil {
		return nil, fmt.Errorf("report %s: %w", name, err)
	}
	return &rep, nil
}

// AssertFullMesh fails unless every (client, server) edge in the
// topology has at least one successful traversal across the given
// reports — the per-edge identity proof the issue demands instead of an
// aggregate request count.
func (r *Run) AssertFullMesh(reports map[string]*report.Report) {
	r.T.Helper()
	for _, client := range r.Topology.Clients {
		rep, ok := reports[client]
		if !ok {
			r.T.Errorf("edge assertion: no report for %s", client)
			continue
		}
		edges := rep.EdgeSuccesses()
		for _, server := range r.Topology.Servers {
			total := 0
			for edge, n := range edges {
				if edge[1] == server {
					total += n
				}
			}
			if total == 0 {
				r.T.Errorf("edge %s -> %s: no successful traversal", client, server)
			}
		}
	}
}

// Logs returns one service's log output so far — the in-test view of
// the access log and lifecycle stderr lines.
func (r *Run) Logs(ctx context.Context, service string) string {
	r.T.Helper()
	out, err := r.Compose.Output(ctx, "logs", "--no-color", service)
	if err != nil {
		r.T.Fatalf("logs of %s: %v", service, err)
	}
	return out
}

// WaitExit blocks until one service's container stops and returns its
// exit code, read from the container state by label filter (stable
// across compose versions, unlike `compose ps` JSON).
func (r *Run) WaitExit(ctx context.Context, service string) int {
	r.T.Helper()
	// `compose wait` propagates the container's exit code as its own, so
	// a non-zero container is not an invocation failure; the code is
	// read authoritatively from inspect below either way.
	_, _ = r.Compose.Output(ctx, "wait", service)
	ids := dockerLines(ctx, r.T, "ps", "-aq",
		"--filter", "label=com.docker.compose.project="+r.Compose.Project,
		"--filter", "label=com.docker.compose.service="+service)
	if len(ids) != 1 {
		r.T.Fatalf("wait for %s: found containers %v", service, ids)
	}
	out := dockerLines(ctx, r.T, "inspect", "--format", "{{.State.ExitCode}}", ids[0])
	if len(out) != 1 {
		r.T.Fatalf("exit code of %s: %v", service, out)
	}
	code, err := strconv.Atoi(out[0])
	if err != nil {
		r.T.Fatalf("exit code of %s: %q", service, out[0])
	}
	return code
}

// teardown collects diagnostics, tears the project down, and proves
// nothing project-owned survived. Ordering is deliberate: diagnostics
// before down (down destroys their sources), the orphan proof after.
func (r *Run) teardown() {
	// Teardown gets its own deadline so it also runs when the test
	// context is already dead.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	r.collectDiagnostics(ctx)
	if err := r.Compose.Down(ctx); err != nil {
		r.T.Errorf("compose down: %v", err)
	}
	r.proveNoOrphans(ctx)
}

// collectDiagnostics snapshots the rendered config, container states,
// and every service's logs into the artifact directory.
func (r *Run) collectDiagnostics(ctx context.Context) {
	for name, args := range map[string][]string{
		"compose.rendered.yml": {"config"},
		"ps.json":              {"ps", "-a", "--format", "json"},
		"logs.txt":             {"logs", "--no-color", "--timestamps"},
	} {
		out, err := r.Compose.Output(ctx, args...)
		if err != nil {
			out += "\ndiagnostic error: " + err.Error()
		}
		if werr := os.WriteFile(filepath.Join(r.ArtifactDir, name), []byte(out), 0o644); werr != nil {
			r.T.Logf("write %s: %v", name, werr)
		}
	}
}

// proveNoOrphans fails the test when any container, network, or volume
// of this project outlived down, then force-removes it so one leak
// cannot cascade into later runs.
func (r *Run) proveNoOrphans(ctx context.Context) {
	const (
		kindNetwork = "network"
		kindVolume  = "volume"
	)
	filter := "label=com.docker.compose.project=" + r.Compose.Project
	checks := []struct {
		kind   string
		list   []string
		remove []string
	}{
		{kind: "container", list: []string{"ps", "-aq"}, remove: []string{"rm", "-f"}},
		{kind: kindNetwork, list: []string{kindNetwork, "ls", "-q"}, remove: []string{kindNetwork, "rm"}},
		{kind: kindVolume, list: []string{kindVolume, "ls", "-q"}, remove: []string{kindVolume, "rm", "-f"}},
	}
	for _, check := range checks {
		ids := dockerLines(ctx, r.T, append(check.list, "--filter", filter)...)
		if len(ids) == 0 {
			continue
		}
		r.T.Errorf("orphan %s(s) after down: %v", check.kind, ids)
		dockerLines(ctx, r.T, append(check.remove, ids...)...)
	}
}

// SweepLaneOrphans is the suite-level epilogue: after everything ran,
// no statute.e2e-labeled container may remain on the host, regardless
// of which run leaked it. It returns the ids it had to reap — a
// non-empty result means the suite must fail — after force-removing
// them so one leak cannot poison the next invocation.
func SweepLaneOrphans(ctx context.Context) []string {
	ids, err := rawDocker(ctx, "ps", "-aq", "--filter", "label=statute.e2e=1")
	if err != nil || len(ids) == 0 {
		return nil
	}
	// Best-effort reap: the returned ids fail the suite either way.
	_, _ = rawDocker(ctx, append([]string{"rm", "-f"}, ids...)...)
	return ids
}

// dockerLines runs one raw docker command (not compose) and returns
// its non-empty output lines, logging failures to the test.
func dockerLines(ctx context.Context, t *testing.T, args ...string) []string {
	t.Helper()
	lines, err := rawDocker(ctx, args...)
	if err != nil {
		t.Logf("docker %s: %v", strings.Join(args, " "), err)
	}
	return lines
}

// rawDocker runs one docker command under the invocation timeout.
func rawDocker(ctx context.Context, args ...string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, composeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		return nil, err
	}
	var lines []string
	for l := range strings.SplitSeq(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines, nil
}

// randomID returns eight hex characters of collision resistance for
// project and artifact naming.
func randomID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// sanitize keeps scenario-derived names safe for Compose project names,
// which allow lowercase alphanumerics, hyphens, and underscores.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, s)
}
