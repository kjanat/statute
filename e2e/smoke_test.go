//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"

	"statute.kjanat.dev/e2e/harness"
	"statute.kjanat.dev/e2e/report"
)

// TestSmoke_Mesh is the PR gate: the mesh scenario across all four
// server/client topologies. Every client exercises every Statute node
// over HTTP/1.1 and TLS/HTTP2 with per-edge identity, one forced
// upstream failover succeeds through Retry, and each run ends with a
// graceful-shutdown proof.
func TestSmoke_Mesh(t *testing.T) {
	for _, topo := range harness.Topologies {
		t.Run(topo.Name, func(t *testing.T) {
			t.Parallel()
			runMeshSmoke(t, topo)
		})
	}
}

func runMeshSmoke(t *testing.T, topo harness.Topology) {
	r := harness.Start(t, "mesh", topo)
	ctx := context.Background()
	r.AwaitReady(ctx)

	reports := make(map[string]*report.Report, len(topo.Clients))
	for _, client := range topo.Clients {
		reports[client] = r.ExecutePlan(ctx, client, meshPlan(client, topo))
	}
	r.AssertFullMesh(reports)
	assertBothOriginsServed(t, topo, reports)
	assertGracefulShutdown(ctx, t, r)
}

// meshPlan sends every client to every server: HTTP/1.1 and TLS/HTTP2
// echo traffic plus one forced-failover request per edge. The failover
// URL arms a one-failure budget on each origin per key; with Retry(3,
// OnStatus(502)) at the route, the third attempt is guaranteed to land
// on an origin whose budget is already spent.
func meshPlan(client string, topo harness.Topology) *report.Plan {
	plan := &report.Plan{Name: "mesh", Seed: 1}
	for _, server := range topo.Servers {
		plan.Steps = append(plan.Steps,
			report.Step{
				Name:         "h1-" + server,
				URL:          fmt.Sprintf("http://%s:%d/echo", server, harness.PortHTTP),
				TargetServer: server,
				Proto:        "h1",
				Count:        8,
				Concurrency:  2,
				// A non-empty echoed request id proves the id the client
				// stamped survived the proxy to the origin.
				Expect: report.Expect{Status: 200, BodyContains: `"request_id":"` + client},
			},
			report.Step{
				Name:         "h2-" + server,
				URL:          fmt.Sprintf("https://%s:%d/echo", server, harness.PortHTTPS),
				TargetServer: server,
				Proto:        "h2",
				Count:        4,
				Concurrency:  2,
				RootsFile:    "/certs/ca.crt",
				Expect:       report.Expect{Status: 200, BodyContains: `"origin":"origin-`},
			},
			report.Step{
				Name:         "failover-" + server,
				URL:          fmt.Sprintf("http://%s:%d/fail?key=%s-%s&n=1", server, harness.PortHTTP, client, server),
				TargetServer: server,
				Proto:        "h1",
				Count:        1,
				Expect:       report.Expect{Status: 200},
			},
		)
	}
	return plan
}

// assertBothOriginsServed proves round-robin distribution per edge:
// across eight HTTP/1.1 requests from one client to one server, both
// origin identities must appear, and HTTP/2 responses must have
// negotiated HTTP/2.0.
func assertBothOriginsServed(t *testing.T, topo harness.Topology, reports map[string]*report.Report) {
	t.Helper()
	for _, client := range topo.Clients {
		rep := reports[client]
		if rep == nil {
			t.Errorf("no report for %s", client)
			continue
		}
		for _, server := range topo.Servers {
			assertEdgeEvidence(t, client, server, rep)
		}
	}
}

func assertEdgeEvidence(t *testing.T, client, server string, rep *report.Report) {
	t.Helper()
	origins := make(map[string]bool)
	for _, res := range rep.Results {
		if res.Step == "h1-"+server && res.OK {
			origins[res.OriginID] = true
		}
		if res.Step == "h2-"+server && res.OK && res.NegotiatedProto != "HTTP/2.0" {
			t.Errorf("%s -> %s: h2 step negotiated %s", client, server, res.NegotiatedProto)
		}
	}
	if !origins["origin-1"] || !origins["origin-2"] {
		t.Errorf("%s -> %s: round robin saw origins %v, want both", client, server, origins)
	}
}

// assertGracefulShutdown terminates statute-1, proves it exits zero and
// releases its listener, and — when the topology has a second node —
// proves statute-2 is untouched by its sibling's shutdown.
func assertGracefulShutdown(ctx context.Context, t *testing.T, r *harness.Run) {
	t.Helper()
	if err := r.Compose.Signal(ctx, harness.Server1, "SIGTERM"); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	if code := r.WaitExit(ctx, harness.Server1); code != 0 {
		t.Errorf("graceful shutdown exit code: %d", code)
	}
	probe := r.Topology.Clients[0]
	url := fmt.Sprintf("http://%s:%d/echo", harness.Server1, harness.PortHTTP)
	if out, err := r.Compose.RunClient(ctx, probe, "probe-negative", "-url", url); err != nil {
		t.Errorf("listener release: %v\n%s", err, out)
	}
	for _, server := range r.Topology.Servers {
		if server == harness.Server1 {
			continue
		}
		ready := fmt.Sprintf("http://%s:%d/healthz/ready", server, harness.PortHealth)
		if out, err := r.Compose.RunClient(ctx, probe, "wait", "-url", ready, "-timeout", "10s"); err != nil {
			t.Errorf("%s after sibling shutdown: %v\n%s", server, err, out)
		}
	}
}
