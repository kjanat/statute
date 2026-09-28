//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"statute.kjanat.dev/e2e/harness"
	"statute.kjanat.dev/e2e/report"
)

// TestRegression_DockerWorkloadConcurrentColdStart proves single-flight through
// actual Docker API requests, and readiness gating through the origin journal.
func TestRegression_DockerWorkloadConcurrentColdStart(t *testing.T) {
	t.Parallel()
	topo := harness.MustTopology(t, "1s1c")
	r := harness.StartServices(t, "workload-concurrency", topo, []string{harness.Server1}, "scenarios/workload-concurrency/compose.yml")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	r.AwaitReady(ctx)
	name := r.Compose.Project + "-cold"
	host := name + ".test"
	network := r.Compose.Project + "_mesh"
	id := strings.TrimSpace(dockerOut(ctx, t, "create", "--name", name, "--network", network, "--entrypoint", "/origin",
		"-e", "ORIGIN_ID=origin-cold", "-e", "ORIGIN_INITIAL_HEALTH=down",
		"--label", "statute.e2e=1", "--label", "statute.enable=true", "--label", "statute.path=/*",
		"--label", "statute.host="+host, "--label", "statute.port=7000",
		"--label", "statute.service="+name, "--label", "statute.network="+network,
		os.Getenv("STATUTE_E2E_IMAGE")))
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		if out, err := exec.CommandContext(cleanup, "docker", "rm", "-f", name).CombinedOutput(); err != nil {
			t.Errorf("remove cold origin: %v: %s", err, out)
		}
	})
	awaitColdWorkload(ctx, t, r, "dormant", 0)
	assertRecordedStarts(ctx, t, r, id, 0)

	const requests = 20
	plan := &report.Plan{Name: "concurrent-cold-start", Seed: 90, Steps: []report.Step{{
		Name: "cold", URL: fmt.Sprintf("http://statute-1:%d/echo", harness.PortHTTP), TargetServer: harness.Server1,
		Proto: "h1", Count: requests, Concurrency: requests,
		Headers: map[string]string{"Host": host},
		Expect:  report.Expect{Status: http.StatusOK, BodyContains: `"origin":"origin-cold"`},
	}}}
	type outcome struct {
		report *report.Report
		err    error
	}
	result := make(chan outcome, 1)
	finished := make(chan struct{})
	planCtx, stopPlan := context.WithCancel(ctx)
	go func() {
		defer close(finished)
		rep, err := r.ExecutePlanE(planCtx, topo.Clients[0], plan)
		result <- outcome{rep, err}
	}()
	t.Cleanup(func() {
		stopPlan()
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Error("cold request driver did not exit after cancellation")
		}
	})

	awaitColdWorkload(ctx, t, r, "starting", requests)
	assertRecordedStarts(ctx, t, r, id, 1)
	planned := make(map[string]bool, requests)
	for i := range requests {
		planned[fmt.Sprintf("%s-cold-90-%d", topo.Clients[0], i)] = true
	}
	assertColdNotForwarded(t, originJournal(ctx, r, name, "http"), planned)
	select {
	case <-finished:
		t.Fatal("cold request batch finished before readiness release")
	default:
	}
	setOriginHealth(ctx, r, name, "up")
	var got outcome
	select {
	case got = <-result:
	case <-ctx.Done():
		t.Fatal("cold requests did not finish: ", ctx.Err())
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	assertColdBatch(t, got.report, planned)
	assertColdJournal(t, originJournal(ctx, r, name, "http"), planned)
	assertRecordedStarts(ctx, t, r, id, 1)
}

func assertColdNotForwarded(t *testing.T, entries []journalEntry, planned map[string]bool) {
	t.Helper()
	for _, entry := range entries {
		if planned[entry.RequestID] {
			t.Fatalf("request %s reached origin before readiness release", entry.RequestID)
		}
	}
}

func assertColdBatch(t *testing.T, rep *report.Report, planned map[string]bool) {
	t.Helper()
	if rep == nil || len(rep.Results) != len(planned) {
		t.Fatalf("report must contain all %d request results: %+v", len(planned), rep)
	}
	seen := make(map[string]int, len(planned))
	for _, res := range rep.Results {
		if !res.OK || !planned[res.RequestID] {
			t.Errorf("cold request failed or has unexpected identity: %+v", res)
		}
		seen[res.RequestID]++
	}
	for id := range planned {
		if seen[id] != 1 {
			t.Errorf("report contains request %s %d times, want once", id, seen[id])
		}
	}
}

func assertColdJournal(t *testing.T, entries []journalEntry, planned map[string]bool) {
	t.Helper()
	journalCounts := make(map[string]int, len(planned))
	for _, entry := range entries {
		if planned[entry.RequestID] {
			journalCounts[entry.RequestID]++
		}
	}
	for id := range planned {
		if journalCounts[id] != 1 {
			t.Errorf("origin received request %s %d times, want once", id, journalCounts[id])
		}
	}
}

func awaitColdWorkload(ctx context.Context, t *testing.T, r *harness.Run, phase string, waiters int) {
	t.Helper()
	pollUntil(t, 20*time.Second, fmt.Sprintf("cold workload %s with %d waiters", phase, waiters), func() (bool, string) {
		body, err := clientGet(ctx, r, fmt.Sprintf("http://statute-1:%d/debug/workloads", harness.PortMetrics))
		if err != nil {
			return false, err.Error()
		}
		var snapshot workloadDiagnosticsReport
		if err := json.Unmarshal([]byte(body), &snapshot); err != nil {
			return false, err.Error()
		}
		if len(snapshot.Services) != 1 || snapshot.Services[0].Service != r.Compose.Project+"-cold" || len(snapshot.Services[0].Owners) != 1 {
			return false, body
		}
		owner := snapshot.Services[0].Owners[0]
		return owner.Current && !owner.Retired && owner.Phase == phase && owner.ActivationWaiters == waiters, body
	})
}

func assertRecordedStarts(ctx context.Context, t *testing.T, r *harness.Run, id string, want uint64) {
	t.Helper()
	body := mustClientGet(ctx, r, "http://dockerproxy:2375/debug/starts")
	var snapshot struct {
		Total  uint64            `json:"total"`
		Starts map[string]uint64 `json:"starts"`
	}
	if err := json.Unmarshal([]byte(body), &snapshot); err != nil {
		t.Fatalf("invalid Docker start counters: %v: %s", err, body)
	}
	if snapshot.Total != want || snapshot.Starts[id] != want {
		t.Fatalf("Docker start requests: %s, want total=%d and immutable %s=%d", body, want, id, want)
	}
}
