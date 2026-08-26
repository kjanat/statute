//go:build e2e

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"statute.kjanat.dev/e2e/report"
)

// startEcho serves a minimal echo-shaped origin on a real loopback
// socket — no httptest in this lane, by contract.
func startEcho(t *testing.T, id string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"origin":%q,"request_id":%q}`, id, r.Header.Get("X-Request-Id"))
	})
	mux.HandleFunc("/broken", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return "http://" + ln.Addr().String()
}

func TestExecutePlanEdgesAndFailures(t *testing.T) {
	t.Parallel()
	base := startEcho(t, "origin-1")
	plan := &report.Plan{
		Name: "unit", ClientID: "client-1", Topology: "1s1c", Seed: 7,
		Steps: []report.Step{
			{
				Name: "ok", URL: base + "/echo", TargetServer: "statute-1", Proto: "h1",
				Count: 4, Concurrency: 2,
				Expect: report.Expect{Status: 200, BodyContains: "origin-1"},
			},
			{
				Name: "broken", URL: base + "/broken", TargetServer: "statute-1", Proto: "h1",
				Count:  2,
				Expect: report.Expect{Status: 200},
			},
		},
	}
	rep := executePlan(plan)
	if len(rep.Results) != 6 {
		t.Fatalf("results: %d", len(rep.Results))
	}
	edges := rep.EdgeSuccesses()
	if edges[[2]string{"ok", "statute-1"}] != 4 {
		t.Errorf("ok edge successes: %v", edges)
	}
	if edges[[2]string{"broken", "statute-1"}] != 0 {
		t.Errorf("broken edge should have no successes: %v", edges)
	}
	if len(rep.Failures) != 1 || rep.Failures[0] != "broken->statute-1" {
		t.Errorf("failures: %v", rep.Failures)
	}
	for _, r := range rep.Results {
		if r.Step == "ok" && (r.OriginID != "origin-1" || r.NegotiatedProto != "HTTP/1.1") {
			t.Errorf("result: %+v", r)
		}
	}
}

// TestExecutePlanUnreachableTargetFails — a dead server is a recorded
// failure, not a crash: exactly the shape the full-mesh assertion needs
// when one topology node never came up.
func TestExecutePlanUnreachableTargetFails(t *testing.T) {
	t.Parallel()
	// Reserve then close a port so the target is provably unreachable.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + ln.Addr().String()
	ln.Close()
	plan := &report.Plan{
		Name: "unit", ClientID: "client-1", Topology: "1s1c",
		Steps: []report.Step{{Name: "down", URL: dead + "/echo", TargetServer: "statute-2", Proto: "h1", Count: 1}},
	}
	rep := executePlan(plan)
	if len(rep.Failures) != 1 {
		t.Fatalf("failures: %v", rep.Failures)
	}
	if rep.Results[0].OK || rep.Results[0].Err == "" {
		t.Errorf("result: %+v", rep.Results[0])
	}
}

func TestStepExpectMet(t *testing.T) {
	t.Parallel()
	cases := []struct {
		expect report.Expect
		status int
		body   string
		want   bool
	}{
		{report.Expect{}, 200, "", true},
		{report.Expect{}, 502, "", false},
		{report.Expect{Status: 502}, 502, "", true},
		{report.Expect{Status: 200}, 404, "", false},
		{report.Expect{BodyContains: "abc"}, 200, "xabcx", true},
		{report.Expect{BodyContains: "abc"}, 200, "nope", false},
	}
	for i, c := range cases {
		step := &report.Step{Expect: c.expect}
		if got := stepExpectMet(step, c.status, []byte(c.body)); got != c.want {
			t.Errorf("case %d: got %v", i, got)
		}
	}
}

// TestReportRoundTrip — the report is an artifact contract between two
// processes; a lossy marshal round-trip would silently break edge
// assertions.
func TestReportRoundTrip(t *testing.T) {
	t.Parallel()
	in := &report.Report{
		ClientID: "client-2", Plan: "mesh", Topology: "2s2c",
		Started: time.Now().Truncate(time.Millisecond).UTC(),
		Results: []report.Result{{
			Step: "s", URL: "http://x/", TargetServer: "statute-1", Proto: "h3",
			NegotiatedProto: "HTTP/3.0", Status: 200, OriginID: "origin-2",
			RequestID: "r1", Latency: 12 * time.Millisecond, OK: true,
		}},
		Failures: []string{"a->b"},
	}
	blob, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out report.Report
	if err := json.Unmarshal(blob, &out); err != nil {
		t.Fatal(err)
	}
	if out.Results[0] != in.Results[0] || out.Failures[0] != "a->b" || out.ClientID != "client-2" {
		t.Errorf("round trip: %+v", out)
	}
}
