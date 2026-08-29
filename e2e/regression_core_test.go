//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"statute.kjanat.dev/e2e/harness"
	"statute.kjanat.dev/e2e/report"
)

// TestRegression_RoutesRewriteRetry proves route matching on the
// original path and exactly-once path rewriting across Retry re-entry,
// from the origin's own journal.
func TestRegression_RoutesRewriteRetry(t *testing.T) {
	t.Parallel()
	topo := harness.MustTopology(t, "1s1c")
	r := harness.Start(t, "routes", topo)
	ctx := context.Background()
	r.AwaitReady(ctx)

	plan := &report.Plan{Name: "routes", Steps: []report.Step{
		{
			Name: "api-echo", URL: "http://statute-1:8080/api/echo", TargetServer: harness.Server1,
			Proto: "h1", Count: 1,
			// The origin must see the stripped path.
			Expect: report.Expect{Status: 200, BodyContains: `"path":"/echo"`},
		},
		{
			Name: "catchall", URL: "http://statute-1:8080/api2/echo", TargetServer: harness.Server1,
			Proto: "h1", Count: 1,
			// The sibling route strips /api; this catch-all must not.
			Expect: report.Expect{Status: 200, BodyContains: `"path":"/api2/echo"`},
		},
		{
			Name: "retry", URL: "http://statute-1:8080/api/fail?key=k1&n=1", TargetServer: harness.Server1,
			Proto: "h1", Count: 1,
			Expect: report.Expect{Status: 200},
		},
	}}
	r.ExecutePlan(ctx, harness.Client1, plan)

	entries := originJournal(ctx, r, "origin-1", "http")
	var failPaths []string
	for _, e := range entries {
		if strings.HasPrefix(e.Query, "key=k1") || strings.Contains(e.Query, "key=k1") {
			failPaths = append(failPaths, e.Path)
		}
		if strings.HasPrefix(e.Path, "/api/") {
			t.Errorf("origin saw unstripped path %q; the rewrite was skipped", e.Path)
		}
		if e.Path == "" || e.Path == "/fail/fail" {
			t.Errorf("origin saw path %q; the rewrite accumulated across retry", e.Path)
		}
	}
	// One failed attempt plus one successful retry, each carrying the
	// once-rewritten path.
	if len(failPaths) != 2 || failPaths[0] != "/fail" || failPaths[1] != "/fail" {
		t.Errorf("retry attempts at origin: %v, want exactly [/fail /fail]", failPaths)
	}
}

// TestRegression_BackendsHealthFailover drives active-health demotion,
// convergence, and recovery through real origin state, on one node and
// on the full 2s2c mesh.
func TestRegression_BackendsHealthFailover(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"1s1c", "2s2c"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			topo := harness.MustTopology(t, name)
			runBackendsHealth(t, topo)
		})
	}
}

func runBackendsHealth(t *testing.T, topo harness.Topology) {
	r := harness.Start(t, "mesh", topo)
	ctx := context.Background()
	r.AwaitReady(ctx)

	echoPlan := func(name string, expectOrigins func(map[string]bool) bool, desc string) {
		t.Helper()
		assertMeshOrigins(ctx, t, r, name, expectOrigins, desc)
	}

	both := func(o map[string]bool) bool { return o["origin-1"] && o["origin-2"] }
	only1 := func(o map[string]bool) bool { return o["origin-1"] && !o["origin-2"] }

	echoPlan("baseline", both, "want both origins")

	// Health is per-node state, so every node must be proven to converge.
	setOriginHealth(ctx, r, "origin-2", "down")
	for _, server := range topo.Servers {
		awaitNodeOrigins(ctx, t, r, server, "demoted", "converges on origin-1", func(got map[string]int) bool {
			return got["origin-1"] == originProbeCount && got["origin-2"] == 0
		})
	}
	echoPlan("converged", only1, "origin-2 is demoted and may not serve")

	setOriginHealth(ctx, r, "origin-2", "up")
	for _, server := range topo.Servers {
		awaitNodeOrigins(ctx, t, r, server, "recovered", "serves origin-2 again", func(got map[string]int) bool {
			return got["origin-2"] > 0
		})
	}
	echoPlan("recovered", both, "want both origins after recovery")
}

// assertMeshOrigins drives every client at every node and checks which
// origins each node served.
func assertMeshOrigins(ctx context.Context, t *testing.T, r *harness.Run, name string, expectOrigins func(map[string]bool) bool, desc string) {
	t.Helper()
	topo := r.Topology
	for _, client := range topo.Clients {
		plan := &report.Plan{Name: name}
		for _, server := range topo.Servers {
			plan.Steps = append(plan.Steps, report.Step{
				Name: name + "-" + server, TargetServer: server, Proto: "h1",
				URL:   fmt.Sprintf("http://%s:%d/echo", server, harness.PortHTTP),
				Count: 8, Concurrency: 2,
				Expect: report.Expect{Status: 200},
			})
		}
		rep := r.ExecutePlan(ctx, client, plan)
		for _, server := range topo.Servers {
			got := make(map[string]bool)
			for _, res := range rep.Results {
				if res.OK && res.TargetServer == server {
					got[res.OriginID] = true
				}
			}
			if !expectOrigins(got) {
				t.Errorf("%s via %s -> %s: origins %v (%s)", name, client, server, got, desc)
			}
		}
	}
}

// originProbeCount is how many consecutive requests one convergence
// probe makes; with two round-robin backends it is far more than enough
// for an eligible origin-2 to appear at least once.
const originProbeCount = 4

// awaitNodeOrigins polls one Statute node with consecutive requests
// until the origins it serves satisfy want. Health is per-node runtime
// state: one node converging proves nothing about its siblings, and a
// fixed sleep proves nothing about either.
func awaitNodeOrigins(ctx context.Context, t *testing.T, r *harness.Run, server, phase, what string, want func(map[string]int) bool) {
	t.Helper()
	plan := &report.Plan{Name: "probe-" + phase + "-" + server, Steps: []report.Step{{
		Name: "echo", TargetServer: server, Proto: "h1",
		URL:   fmt.Sprintf("http://%s:%d/echo", server, harness.PortHTTP),
		Count: originProbeCount, Concurrency: 1,
		Expect: report.Expect{Status: 200},
	}}}
	pollUntil(t, 60*time.Second, server+" "+what, func() (bool, string) {
		rep, err := r.ExecutePlanE(ctx, r.Topology.Clients[0], plan)
		if err != nil {
			return false, err.Error()
		}
		got := make(map[string]int)
		for _, res := range rep.Results {
			if res.OK {
				got[res.OriginID]++
			}
		}
		return want(got), fmt.Sprintf("%v", got)
	})
}

// TestRegression_UpstreamTLSParity proves backend TLS verification, client
// identity, and Host policy hold identically for proxied requests and active
// health probes, and that a route whose pool cannot verify its backend fails
// closed.
func TestRegression_UpstreamTLSParity(t *testing.T) {
	t.Parallel()
	topo := harness.MustTopology(t, "1s1c")
	r := harness.Start(t, "upstream-tls", topo, "scenarios/upstream-tls/compose.yml")
	ctx := context.Background()
	r.AwaitReady(ctx)

	r.ExecutePlan(ctx, harness.Client1, &report.Plan{Name: "tls", Steps: []report.Step{
		{
			Name: "good", URL: "http://statute-1:8080/good/echo", TargetServer: harness.Server1,
			Proto: "h1", Count: 4,
			Expect: report.Expect{Status: 200, BodyContains: `"sni":"origin-1"`},
		},
		{
			Name: "bad", URL: "http://statute-1:8080/bad/echo", TargetServer: harness.Server1,
			Proto: "h1", Count: 2,
			// The mis-rooted pool must fail closed, never fall back to
			// plaintext or unverified TLS.
			Expect: report.Expect{Status: 502},
		},
	}})

	// Parity: proxied traffic and health probes carry the same SNI and
	// Host at the origin; the probe lands on the pool's 2s ticker.
	entries := awaitOriginJournal(ctx, t, r, "origin-1", "https",
		"origin-1 journal carries both proxy traffic and a health probe", bothTrafficKinds)
	assertJournalParity(t, entries, "origin-1", "origin-1:7000")

	// Verification fails during handshake, so no request ever completes
	// at origin-2.
	for _, e := range originJournal(ctx, r, "origin-2", "https") {
		if e.Path == "/echo" {
			t.Errorf("origin-2 served %q despite an unverifiable pool root", e.Path)
		}
	}
}

// bothTrafficKinds accepts a journal snapshot only once it holds both
// proxied traffic and an active health probe, so the parity assertion
// compares two kinds that are genuinely present rather than passing
// vacuously on whichever kind arrived first.
func bothTrafficKinds(entries []journalEntry) (bool, string) {
	var echo, probe bool
	for _, e := range entries {
		switch e.Path {
		case "/echo":
			echo = true
		case "/health":
			probe = true
		}
	}
	return echo && probe, fmt.Sprintf("echo=%v probe=%v", echo, probe)
}

// assertJournalParity requires every proxied and probe entry at one
// origin to carry the same SNI and Host: the pool owns one transport and
// one UpstreamHost policy, so health traffic may never diverge from real
// traffic.
func assertJournalParity(t *testing.T, entries []journalEntry, origin, host string) {
	t.Helper()
	for _, e := range entries {
		if e.Path != "/echo" && e.Path != "/health" {
			continue
		}
		if e.SNI != origin || e.Host != host {
			t.Errorf("%s at %s: SNI %q Host %q; proxy and probe policy must match TargetHost", e.Path, origin, e.SNI, e.Host)
		}
	}
}

// TestRegression_HTTP3 proves real HTTP/1.1, HTTP/2, and HTTP/3 service
// and that shutdown releases the QUIC UDP listener as well as TCP.
func TestRegression_HTTP3(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"1s1c", "2s1c"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			topo := harness.MustTopology(t, name)
			runHTTP3(t, topo)
		})
	}
}

// TestRegression_ClientMTLS proves listener-wide client authentication over
// each supported HTTPS protocol and rejection before ordinary routing.
func TestRegression_ClientMTLS(t *testing.T) {
	t.Parallel()
	topo := harness.MustTopology(t, "1s1c")
	r := harness.Start(t, "client-mtls", topo)
	ctx := context.Background()
	r.AwaitReady(ctx)

	rep := r.ExecutePlan(ctx, harness.Client1, clientMTLSPlan())
	assertClientMTLSProtocols(t, rep)
	assertMissingClientCertRejected(ctx, t, r)
	assertVerifiedClientCertLogged(ctx, t, r)
}

func clientMTLSPlan() *report.Plan {
	plan := &report.Plan{Name: "client-mtls"}
	for _, proto := range []string{"h1", "h2", "h3"} {
		plan.Steps = append(plan.Steps, report.Step{
			Name: proto, URL: "https://statute-1:8443/echo", TargetServer: harness.Server1,
			Proto: proto, Count: 1, RootsFile: "/certs/ca.crt",
			ClientCertFile: "/certs/statute.crt", ClientKeyFile: "/certs/statute.key",
			Expect: report.Expect{Status: 200, BodyContains: `"origin":"origin-1"`},
		})
	}
	return plan
}

func assertClientMTLSProtocols(t *testing.T, rep *report.Report) {
	t.Helper()
	for _, result := range rep.Results {
		if result.Proto == "h2" && result.NegotiatedProto != "HTTP/2.0" {
			t.Errorf("h2 step negotiated %q", result.NegotiatedProto)
		}
		if result.Proto == "h3" && result.NegotiatedProto != "HTTP/3.0" {
			t.Errorf("h3 step negotiated %q", result.NegotiatedProto)
		}
	}
}

func assertMissingClientCertRejected(ctx context.Context, t *testing.T, r *harness.Run) {
	t.Helper()
	for _, proto := range []string{"h1", "h2", "h3"} {
		out, err := r.Compose.RunClient(ctx, harness.Client1,
			"probe-negative", "-url", "https://statute-1:8443/echo",
			"-proto", proto, "-roots", "/certs/ca.crt")
		if err != nil {
			t.Errorf("missing certificate over %s was not rejected: %v\n%s", proto, err, out)
		}
	}
}

func assertVerifiedClientCertLogged(ctx context.Context, t *testing.T, r *harness.Run) {
	t.Helper()
	logs := awaitServiceLog(ctx, t, r, harness.Server1, "verified client identity in access log", func(logs string) (bool, string) {
		return strings.Contains(logs, `"client_cert_subject":"CN=statute-e2e-proxy"`), logs
	})
	if strings.Contains(logs, `"path":"/echo"`) && !strings.Contains(logs, `"client_cert_sans"`) {
		t.Errorf("verified client access log has no SANs:\n%s", logs)
	}
}

func runHTTP3(t *testing.T, topo harness.Topology) {
	r := harness.Start(t, "h3", topo)
	ctx := context.Background()
	r.AwaitReady(ctx)

	plan := &report.Plan{Name: "h3"}
	for _, server := range topo.Servers {
		for _, proto := range []string{"h1", "h2", "h3"} {
			url := fmt.Sprintf("https://%s:%d/echo", server, harness.PortHTTPS)
			if proto == "h1" {
				url = fmt.Sprintf("http://%s:%d/echo", server, harness.PortHTTP)
			}
			plan.Steps = append(plan.Steps, report.Step{
				Name: proto + "-" + server, URL: url, TargetServer: server, Proto: proto,
				Count: 2, RootsFile: "/certs/ca.crt",
				Expect: report.Expect{Status: 200, BodyContains: `"origin":"origin-`},
			})
		}
	}
	rep := r.ExecutePlan(ctx, harness.Client1, plan)
	for _, res := range rep.Results {
		if res.Proto == "h3" && res.OK && res.NegotiatedProto != "HTTP/3.0" {
			t.Errorf("h3 step negotiated %s", res.NegotiatedProto)
		}
	}

	assertSocketRelease(ctx, t, r)
}

// assertSocketRelease terminates statute-1 and proves both socket kinds
// are gone; the QUIC probe would hang forever on a leaked UDP
// listener's silence, so unreachability is its timeout.
func assertSocketRelease(ctx context.Context, t *testing.T, r *harness.Run) {
	t.Helper()
	if err := r.Compose.Signal(ctx, harness.Server1, "SIGTERM"); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	if code := r.WaitExit(ctx, harness.Server1); code != 0 {
		t.Errorf("shutdown exit code: %d", code)
	}
	for _, probe := range [][]string{
		{"probe-negative", "-url", fmt.Sprintf("http://%s:%d/echo", harness.Server1, harness.PortHTTP), "-proto", "h1"},
		{"probe-negative", "-url", fmt.Sprintf("https://%s:%d/echo", harness.Server1, harness.PortHTTPS), "-proto", "h3", "-roots", "/certs/ca.crt"},
	} {
		if out, err := r.Compose.RunClient(ctx, harness.Client1, probe...); err != nil {
			t.Errorf("release proof %v: %v\n%s", probe[2], err, out)
		}
	}
}

// TestRegression_StreamingAndUpgrade proves chunked responses stream
// through the proxy (first chunk long before completion) and a
// connection upgrade round-trips raw bytes both ways.
func TestRegression_StreamingAndUpgrade(t *testing.T) {
	t.Parallel()
	topo := harness.MustTopology(t, "1s1c")
	// The h3 scenario's route is Retry-free: Retry(OnStatus) buffers the
	// response, defeating streaming and hiding http.Hijacker.
	r := harness.Start(t, "h3", topo)
	ctx := context.Background()
	r.AwaitReady(ctx)

	url := fmt.Sprintf("http://%s:%d/stream?chunks=5&interval=300ms", harness.Server1, harness.PortHTTP)
	if out, err := r.Compose.RunClient(ctx, harness.Client1, "stream", "-url", url, "-chunks", "5"); err != nil {
		t.Errorf("streaming: %v\n%s", err, out)
	}
	up := fmt.Sprintf("http://%s:%d/upgrade", harness.Server1, harness.PortHTTP)
	if out, err := r.Compose.RunClient(ctx, harness.Client1, "upgrade", "-url", up, "-lines", "3"); err != nil {
		t.Errorf("upgrade: %v\n%s", err, out)
	}
}

// TestRegression_StartupRetry proves a failed startup exposes no route
// and exits non-zero, and that a corrected configuration in the same
// service slot actually serves — not merely starts.
func TestRegression_StartupRetry(t *testing.T) {
	t.Parallel()
	topo := harness.MustTopology(t, "1s1c")
	r := harness.StartServices(t, "startup-bad", topo, []string{"origin-1", "origin-2"})
	ctx := context.Background()

	if err := r.Compose.UpDetached(ctx, harness.Server1); err != nil {
		t.Fatalf("up: %v", err)
	}
	if code := r.WaitExit(ctx, harness.Server1); code == 0 {
		t.Fatal("startup with missing TLS material exited 0")
	}
	probe := fmt.Sprintf("http://%s:%d/echo", harness.Server1, harness.PortHTTP)
	if out, err := r.Compose.RunClient(ctx, harness.Client1, "probe-negative", "-url", probe); err != nil {
		t.Errorf("failed startup exposed a route: %v\n%s", err, out)
	}

	// The corrected retry: same service slot, fixed configuration.
	r.Compose.Env["STATUTE_SCENARIO"] = "mesh"
	if err := r.Compose.Up(ctx, harness.Server1); err != nil {
		t.Fatalf("corrected up: %v", err)
	}
	r.AwaitReady(ctx)
	r.ExecutePlan(ctx, harness.Client1, &report.Plan{Name: "after-retry", Steps: []report.Step{{
		Name: "echo", URL: probe, TargetServer: harness.Server1, Proto: "h1", Count: 2,
		Expect: report.Expect{Status: 200, BodyContains: `"origin":"origin-`},
	}}})
}

// TestRegression_ShutdownInFlight proves an in-flight slow request
// completes across SIGTERM while the drain refuses new work and the
// process exits within its grace period.
func TestRegression_ShutdownInFlight(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"1s1c", "1s2c"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			topo := harness.MustTopology(t, name)
			runShutdownInFlight(t, topo)
		})
	}
}

func runShutdownInFlight(t *testing.T, topo harness.Topology) {
	r := harness.Start(t, "mesh", topo)
	ctx := context.Background()
	r.AwaitReady(ctx)

	// Must fit inside the 10s grace with room to spare: Go's 500ms
	// shutdown poll alone overruns a drain that needs the whole grace.
	slow := fmt.Sprintf("http://%s:%d/slow?d=5s", harness.Server1, harness.PortHTTP)
	var (
		wg      sync.WaitGroup
		slowOut string
		slowErr error
	)
	wg.Go(func() {
		slowOut, slowErr = clientGet(ctx, r, slow)
	})
	// Drain only once the request is provably at the origin; a fixed
	// sleep races one-shot container startup under load.
	awaitOriginPath(ctx, t, r, "/slow")
	if err := r.Compose.Signal(ctx, harness.Server1, "SIGTERM"); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	wg.Wait()
	if slowErr != nil {
		t.Errorf("in-flight request across SIGTERM: %v\n%s", slowErr, slowOut)
	}
	if !strings.Contains(slowOut, `"origin":"origin-`) {
		t.Errorf("in-flight response body: %q", slowOut)
	}
	if code := r.WaitExit(ctx, harness.Server1); code != 0 {
		t.Errorf("drain exit code: %d", code)
	}
	probe := fmt.Sprintf("http://%s:%d/echo", harness.Server1, harness.PortHTTP)
	if out, err := r.Compose.RunClient(ctx, harness.Client1, "probe-negative", "-url", probe); err != nil {
		t.Errorf("listener after drain: %v\n%s", err, out)
	}
}

// TestRegression_NodeStateIsolation proves runtime health state is
// per-node: passive demotion fed only through statute-1 changes its
// backend selection while statute-2, sharing the same origins, keeps
// serving both.
func TestRegression_NodeStateIsolation(t *testing.T) {
	t.Parallel()
	topo := harness.MustTopology(t, "2s2c")
	r := harness.Start(t, "isolation", topo)
	ctx := context.Background()
	r.AwaitReady(ctx)

	// Only origin-1 fails these, so round-robin feeds it past
	// MaxFailures; the step asserts no status because 502s are expected.
	r.ExecutePlan(ctx, harness.Client1, &report.Plan{Name: "perturb", Steps: []report.Step{{
		Name:         "fail-origin-1",
		URL:          fmt.Sprintf("http://%s:%d/fail?origin=origin-1&key=iso&n=100", harness.Server1, harness.PortHTTP),
		TargetServer: harness.Server1, Proto: "h1", Count: 10,
	}}})

	pollUntil(t, 20*time.Second, "statute-1 demotes origin-1", func() (bool, string) {
		body := mustClientGet(ctx, r, fmt.Sprintf("http://%s:%d/echo", harness.Server1, harness.PortHTTP))
		return strings.Contains(body, `"origin":"origin-2"`), body
	})

	seen := func(server, client string) map[string]bool {
		rep := r.ExecutePlan(ctx, client, &report.Plan{Name: "observe-" + server, Steps: []report.Step{{
			Name: "echo", URL: fmt.Sprintf("http://%s:%d/echo", server, harness.PortHTTP),
			TargetServer: server, Proto: "h1", Count: 8,
			Expect: report.Expect{Status: 200},
		}}})
		got := make(map[string]bool)
		for _, res := range rep.Results {
			if res.OK {
				got[res.OriginID] = true
			}
		}
		return got
	}

	if got := seen(harness.Server1, harness.Client1); got["origin-1"] {
		t.Errorf("statute-1 still serves origin-1 after passive demotion: %v", got)
	}
	if got := seen(harness.Server2, harness.Client2); !got["origin-1"] || !got["origin-2"] {
		t.Errorf("statute-2 origins %v; its sibling's demotion leaked across nodes", got)
	}
}

// TestRegression_TrustedClientIdentity proves forwarded-header trust is
// per-listener: a spoofed X-Forwarded-For from an untrusted peer keeps
// the socket address in the access log, while the trusted listener
// attributes the forwarded client.
func TestRegression_TrustedClientIdentity(t *testing.T) {
	t.Parallel()
	topo := harness.MustTopology(t, "1s1c")
	r := harness.Start(t, "trusted", topo)
	ctx := context.Background()
	r.AwaitReady(ctx)

	spoof := map[string]string{"X-Forwarded-For": "203.0.113.9"}
	r.ExecutePlan(ctx, harness.Client1, &report.Plan{Name: "identity", Steps: []report.Step{
		{
			Name: "untrusted", URL: fmt.Sprintf("http://statute-1:%d/echo?leg=untrusted", harness.PortHTTP),
			TargetServer: harness.Server1, Proto: "h1", Count: 1, Headers: spoof,
			Expect: report.Expect{Status: 200},
		},
		{
			Name: "trusted", URL: fmt.Sprintf("https://statute-1:%d/echo?leg=trusted", harness.PortHTTPS),
			TargetServer: harness.Server1, Proto: "h1", Count: 1, Headers: spoof,
			RootsFile: "/certs/ca.crt",
			Expect:    report.Expect{Status: 200},
		},
	}})

	// The access log is not on stdout merely because the plan returned;
	// wait for both legs, then assert their exact count on that snapshot.
	logs := awaitServiceLog(ctx, t, r, harness.Server1,
		"both access log legs reach statute-1's log", func(logs string) (bool, string) {
			u, tr := logLinesContaining(logs, "leg=untrusted", `"remote"`), logLinesContaining(logs, "leg=trusted", `"remote"`)
			return len(u) > 0 && len(tr) > 0, fmt.Sprintf("untrusted=%d trusted=%d", len(u), len(tr))
		})
	untrusted := logLinesContaining(logs, "leg=untrusted", `"remote"`)
	trusted := logLinesContaining(logs, "leg=trusted", `"remote"`)
	if len(untrusted) != 1 || len(trusted) != 1 {
		t.Fatalf("access log lines: untrusted=%d trusted=%d\n%s", len(untrusted), len(trusted), logs)
	}
	if strings.Contains(untrusted[0], `"remote":"203.0.113.9"`) {
		t.Errorf("untrusted listener honored a spoofed forwarded header: %s", untrusted[0])
	}
	if !strings.Contains(trusted[0], `"remote":"203.0.113.9"`) {
		t.Errorf("trusted listener ignored the forwarded client: %s", trusted[0])
	}
}
