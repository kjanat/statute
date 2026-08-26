//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"

	"statute.kjanat.dev/e2e/harness"
	"statute.kjanat.dev/e2e/report"
)

// TestSoak_MeshSustained drives sustained concurrent full-mesh traffic
// on the largest topology; connection reuse happens inside each step's
// shared transport, and steady state tolerates zero failed requests.
func TestSoak_MeshSustained(t *testing.T) {
	t.Parallel()
	topo, _ := harness.TopologyByName("2s2c")
	r := harness.Start(t, "mesh", topo)
	ctx := context.Background()
	r.AwaitReady(ctx)

	// The clients are independent: their plans run concurrently so the
	// mesh actually carries sustained simultaneous load.
	type outcome struct {
		client string
		rep    *report.Report
		err    error
	}
	done := make(chan outcome, len(topo.Clients))
	for _, client := range topo.Clients {
		plan := &report.Plan{Name: "sustained"}
		for _, server := range topo.Servers {
			plan.Steps = append(plan.Steps,
				report.Step{
					Name: "h1-" + server, TargetServer: server, Proto: "h1",
					URL:   fmt.Sprintf("http://%s:%d/echo", server, harness.PortHTTP),
					Count: 200, Concurrency: 8,
					Expect: report.Expect{Status: 200, BodyContains: `"origin":"origin-`},
				},
				report.Step{
					Name: "h2-" + server, TargetServer: server, Proto: "h2",
					URL:   fmt.Sprintf("https://%s:%d/echo", server, harness.PortHTTPS),
					Count: 100, Concurrency: 8, RootsFile: "/certs/ca.crt",
					Expect: report.Expect{Status: 200, BodyContains: `"origin":"origin-`},
				},
			)
		}
		go func() {
			rep, err := r.ExecutePlanE(ctx, client, plan)
			done <- outcome{client: client, rep: rep, err: err}
		}()
	}
	reports := make(map[string]*report.Report, len(topo.Clients))
	for range topo.Clients {
		got := <-done
		if got.err != nil {
			t.Errorf("steady state: %s: %v", got.client, got.err)
			continue
		}
		for _, res := range got.rep.Results {
			if !res.OK {
				t.Errorf("steady state: %s -> %s: %s", got.client, res.TargetServer, res.Err)
			}
		}
		reports[got.client] = got.rep
	}
	r.AssertFullMesh(reports)
}

// TestSoak_RollingRestartUnderLoad restarts statute-1 repeatedly while
// its sibling carries traffic: the stable node must stay perfect
// throughout, and the restarted node must serve again after every
// cycle.
func TestSoak_RollingRestartUnderLoad(t *testing.T) {
	t.Parallel()
	topo, _ := harness.TopologyByName("2s1c")
	r := harness.Start(t, "mesh", topo)
	ctx := context.Background()
	r.AwaitReady(ctx)

	// ~15s of continuous traffic: it must outlast the journal poll plus
	// the restart, both docker CLI round trips costing seconds, or the
	// restart would land after the client already exited.
	stablePlan := func(name, marker string) *report.Plan {
		return &report.Plan{Name: name, Steps: []report.Step{{
			Name: "stable", TargetServer: harness.Server2, Proto: "h1",
			URL:   fmt.Sprintf("http://%s:%d/slow?d=1s&%s", harness.Server2, harness.PortHTTP, marker),
			Count: 60, Concurrency: 4,
			Expect: report.Expect{Status: 200},
		}}}
	}
	type outcome struct {
		rep *report.Report
		err error
	}
	for cycle := range 3 {
		marker := fmt.Sprintf("cycle=%d", cycle)
		done := make(chan outcome, 1)
		go func() {
			rep, err := r.ExecutePlanE(ctx, harness.Client1, stablePlan(fmt.Sprintf("cycle-%d", cycle), marker))
			done <- outcome{rep: rep, err: err}
		}()
		// The origin journals a request on arrival, before its 1s hold,
		// so the marker proves this cycle's traffic is in flight now.
		awaitOriginQuery(ctx, t, r, marker)
		if err := r.Compose.Restart(ctx, harness.Server1); err != nil {
			t.Fatalf("cycle %d restart: %v", cycle, err)
		}
		got := <-done
		if got.err != nil {
			t.Fatalf("cycle %d: stable plan: %v", cycle, got.err)
		}
		for _, res := range got.rep.Results {
			if !res.OK {
				t.Errorf("cycle %d: stable node failed a request: %s", cycle, res.Err)
			}
		}
		// The restarted node must actually serve, not merely run.
		ready := fmt.Sprintf("http://%s:%d/healthz/ready", harness.Server1, harness.PortHealth)
		if out, err := r.Compose.RunClient(ctx, harness.Client1, "wait", "-url", ready, "-timeout", "30s"); err != nil {
			t.Fatalf("cycle %d: restarted node not serving: %v\n%s", cycle, err, out)
		}
	}
}

// TestSoak_SlowStreamAcrossShutdown holds a slow streaming response
// open while the node drains: the stream must complete and the drain
// must still finish inside the grace period.
func TestSoak_SlowStreamAcrossShutdown(t *testing.T) {
	t.Parallel()
	topo, _ := harness.TopologyByName("1s1c")
	r := harness.Start(t, "h3", topo)
	ctx := context.Background()
	r.AwaitReady(ctx)

	url := fmt.Sprintf("http://%s:%d/stream?chunks=8&interval=1s", harness.Server1, harness.PortHTTP)
	done := make(chan error, 1)
	go func() {
		_, err := r.Compose.RunClient(ctx, harness.Client1, "stream", "-url", url, "-chunks", "8")
		done <- err
	}()
	// Wait until the stream is genuinely flowing at an origin, then
	// drain the node under it.
	awaitOriginPath(ctx, t, r, "/stream")
	if err := r.Compose.Signal(ctx, harness.Server1, "SIGTERM"); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	if err := <-done; err != nil {
		t.Errorf("stream across shutdown: %v", err)
	}
	if code := r.WaitExit(ctx, harness.Server1); code != 0 {
		t.Errorf("drain exit code: %d", code)
	}
}
