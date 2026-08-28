//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"statute.kjanat.dev/e2e/harness"
	"statute.kjanat.dev/e2e/report"
)

// dockerCLI runs one raw docker command for the discovery scenario's
// container churn and fails the test on error.
func dockerCLI(ctx context.Context, t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// TestRegression_DockerDiscovery proves label discovery against a real
// Docker Engine: a labeled container becomes a route with its exact-key
// code-owned pool policy, replacing it swaps the dynamic generation,
// removal withdraws it, and the compiled static route shadows the
// label-derived catch-all throughout.
func TestRegression_DockerDiscovery(t *testing.T) {
	t.Parallel()
	topo := harness.MustTopology(t, "1s1c")
	r := harness.Start(t, "docker", topo, "scenarios/docker/compose.yml")
	ctx := context.Background()
	r.AwaitReady(ctx)

	network := r.Compose.Project + "_mesh"
	image := os.Getenv("STATUTE_E2E_IMAGE")
	launch := func(name, originID string) {
		t.Helper()
		dockerCLI(ctx, t, "run", "-d", "--name", name,
			"--network", network,
			"--entrypoint", "/origin",
			"-e", "ORIGIN_ID="+originID,
			"-e", "ORIGIN_ADDR=:7000",
			"--label", "statute.e2e=1",
			"--label", "statute.enable=true",
			"--label", "statute.path=/*",
			"--label", "statute.port=7000",
			"--label", "statute.service=dyn",
			"--label", "statute.network="+network,
			image)
		t.Cleanup(func() {
			exec.Command("docker", "rm", "-f", name).Run()
		})
	}
	dynBody := func() string {
		out, err := clientGet(ctx, r, fmt.Sprintf("http://statute-1:%d/echo", harness.PortHTTP))
		if err != nil {
			return "error: " + err.Error() + out
		}
		return out
	}
	assertStatic := func() {
		t.Helper()
		body := mustClientGet(ctx, r, fmt.Sprintf("http://statute-1:%d/static/echo", harness.PortHTTP))
		if !strings.Contains(body, `"origin":"origin-1"`) {
			t.Errorf("static route: %q; a dynamic catch-all shadowed compiled configuration", body)
		}
	}

	// Before discovery: only the static route serves.
	assertStatic()
	if out, err := clientGet(ctx, r, fmt.Sprintf("http://statute-1:%d/echo", harness.PortHTTP)); err == nil {
		t.Fatalf("dynamic path served before any labeled container existed: %s", out)
	}

	dyn1 := r.Compose.Project + "-dyn-1"
	launch(dyn1, "origin-dyn1")
	pollUntil(t, 30*time.Second, "labeled container becomes a route", func() (bool, string) {
		b := dynBody()
		return strings.Contains(b, `"origin":"origin-dyn1"`) && strings.Contains(b, `"host":"policy.internal"`), b
	})
	assertStatic()

	// Generation replacement: a successor container with the same labels
	// takes over; the retired generation must not serve again.
	dyn2 := r.Compose.Project + "-dyn-2"
	launch(dyn2, "origin-dyn2")
	dockerCLI(ctx, t, "rm", "-f", dyn1)
	pollUntil(t, 30*time.Second, "replacement generation serves", func() (bool, string) {
		b := dynBody()
		return strings.Contains(b, `"origin":"origin-dyn2"`) && strings.Contains(b, `"host":"policy.internal"`), b
	})
	assertStatic()

	// Withdrawal: removing the last labeled container withdraws the
	// route entirely.
	dockerCLI(ctx, t, "rm", "-f", dyn2)
	pollUntil(t, 30*time.Second, "route withdrawn", func() (bool, string) {
		out, err := clientGet(ctx, r, fmt.Sprintf("http://statute-1:%d/echo", harness.PortHTTP))
		return err != nil, out
	})
	assertStatic()
}

// TestRegression_ACMEHTTP01 proves a hermetic ACME issuance: Pebble as
// the CA through the Directory override, HTTP-01 tokens served on the
// plain listener, and the issued certificate actually terminating TLS
// for the domain.
func TestRegression_ACMEHTTP01(t *testing.T) {
	t.Parallel()
	topo := harness.MustTopology(t, "1s1c")
	services := []string{harness.Server1, "origin-1", "origin-2", "pebble"}
	r := harness.StartServices(t, "acme-http01", topo, services, "scenarios/acme-http01/compose.yml")
	ctx := context.Background()
	r.AwaitReady(ctx)

	// Pebble mints its issuance root per run, so the client cannot verify
	// the served certificate until it has fetched this one.
	if out, err := r.Compose.RunClient(ctx, harness.Client1, "fetch-roots",
		"-url", "https://pebble:15000/roots/0", "-insecure", "-out", "/reports/pebble-root.pem"); err != nil {
		t.Fatalf("fetch pebble root: %v\n%s", err, out)
	}

	// Issuance happens on demand; the wait gives the order, validation,
	// and warm-up time to complete before the strict plan runs.
	target := fmt.Sprintf("https://proxy.e2e.test:%d/echo", harness.PortHTTPS)
	if out, err := r.Compose.RunClient(ctx, harness.Client1, "wait",
		"-url", target, "-roots", "/reports/pebble-root.pem", "-timeout", "90s"); err != nil {
		t.Fatalf("first TLS response with a Pebble-issued certificate: %v\n%s", err, out)
	}

	r.ExecutePlan(ctx, harness.Client1, &report.Plan{Name: "acme", Steps: []report.Step{{
		Name: "tls", URL: target, TargetServer: harness.Server1, Proto: "h1", Count: 3,
		RootsFile: "/reports/pebble-root.pem",
		Expect:    report.Expect{Status: 200, BodyContains: `"origin":"origin-1"`},
	}}})
}

// TestRegression_Observability correlates one request identifier from
// the client report through the access log and the origin journal, and
// proves metrics and a span for the traffic reached their real
// endpoints.
func TestRegression_Observability(t *testing.T) {
	t.Parallel()
	topo := harness.MustTopology(t, "1s1c")
	services := []string{harness.Server1, "origin-1", "origin-2", "otelcol"}
	r := harness.StartServices(t, "observability", topo, services, "scenarios/observability/compose.yml")
	ctx := context.Background()
	r.AwaitReady(ctx)

	rep := r.ExecutePlan(ctx, harness.Client1, &report.Plan{Name: "obs", Steps: []report.Step{{
		Name: "echo", URL: fmt.Sprintf("http://statute-1:%d/echo?probe=trace", harness.PortHTTP),
		TargetServer: harness.Server1, Proto: "h1", Count: 3,
		Expect: report.Expect{Status: 200},
	}}})
	if len(rep.Results) == 0 {
		t.Fatal("client report holds no results")
	}
	requestID := rep.Results[0].RequestID
	if requestID == "" {
		t.Fatal("client report carries no request id")
	}

	// The same identifier in the access log, which is not on stdout
	// merely because the plan returned.
	awaitServiceLog(ctx, t, r, harness.Server1,
		"access log carries request "+requestID, func(logs string) (bool, string) {
			return len(logLinesContaining(logs, requestID, "probe=trace")) > 0, logs
		})
	// ...and in the origin's own journal.
	var atOrigin bool
	for _, e := range originJournal(ctx, r, "origin-1", "http") {
		if e.RequestID == requestID {
			atOrigin = true
			break
		}
	}
	if !atOrigin {
		t.Errorf("origin journal holds no entry for request %s", requestID)
	}

	// Prometheus serves real counters for the traffic.
	metrics := mustClientGet(ctx, r, fmt.Sprintf("http://statute-1:%d/metrics", harness.PortMetrics))
	if !strings.Contains(metrics, "statute") {
		t.Errorf("metrics endpoint exposes no statute series:\n%.1000s", metrics)
	}

	// A span from this service reached the collector; the file exporter
	// writes onto the shared mount.
	traceFile := filepath.Join(r.ReportsDir, "otel-traces.json")
	pollUntil(t, 30*time.Second, "span reaches the collector", func() (bool, string) {
		blob, err := os.ReadFile(traceFile)
		if err != nil {
			return false, err.Error()
		}
		s := string(blob)
		return strings.Contains(s, "statute-e2e") && strings.Contains(s, "/echo"), fmt.Sprintf("%d bytes", len(blob))
	})
}
