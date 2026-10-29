//go:build e2e

package main

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"statute.kjanat.dev/e2e/report"
)

func TestPlanWaitThenFetch(t *testing.T) {
	t.Parallel()
	base, fetches := startPlanWaitServer(t, true)
	rep := executePlan(waitFetchPlan(base, "2s"))
	if len(rep.Failures) != 0 || len(rep.Results) != 2 || fetches.Load() != 1 {
		t.Fatalf("wait/fetch: report=%+v fetches=%d", rep, fetches.Load())
	}
	if rep.Results[0].Body != `{"ready":true}` || rep.Results[0].Status != http.StatusOK || !rep.Results[1].OK {
		t.Fatalf("wait response or fetch missing: %+v", rep.Results)
	}
}

func TestPlanFailedWaitStopsBeforeFetch(t *testing.T) {
	t.Parallel()
	base, fetches := startPlanWaitServer(t, false)
	rep := executePlan(waitFetchPlan(base, "30ms"))
	if len(rep.Failures) != 1 || len(rep.Results) != 1 || rep.Results[0].OK ||
		!strings.Contains(rep.Results[0].Err, "deadline exceeded") || fetches.Load() != 0 {
		t.Fatalf("failed wait allowed fetch: report=%+v fetches=%d", rep, fetches.Load())
	}
}

func waitFetchPlan(base, timeout string) *report.Plan {
	return &report.Plan{Steps: []report.Step{
		{Name: "ready", URL: base + "/state", Wait: &report.WaitStep{
			Timeout: timeout, Spec: report.WaitSpec{Contains: []string{`"ready":true`}},
		}},
		{Name: "fetch", URL: base + "/content", Count: 1, Expect: report.Expect{Status: 200, BodyContains: "content"}},
	}}
}

func startPlanWaitServer(t *testing.T, ready bool) (string, *atomic.Int32) {
	t.Helper()
	var polls, fetches atomic.Int32
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/state", func(w http.ResponseWriter, _ *http.Request) {
		poll := polls.Add(1)
		fmt.Fprintf(w, `{"ready":%t}`, ready && poll >= 2)
	})
	mux.HandleFunc("/content", func(w http.ResponseWriter, _ *http.Request) {
		fetches.Add(1)
		if polls.Load() < 2 {
			http.Error(w, "fetch before readiness", http.StatusConflict)
			return
		}
		fmt.Fprint(w, "content")
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return "http://" + ln.Addr().String(), &fetches
}

func TestPlanWaitRejectsInvalidSteps(t *testing.T) {
	t.Parallel()
	for _, change := range []func(*report.Step){
		func(s *report.Step) { s.Wait.Timeout = "invalid" },
		func(s *report.Step) { s.Wait.Timeout = "0s" },
		func(s *report.Step) { s.Wait.Spec.Status = 99 },
		func(s *report.Step) { s.Count = 2 },
		func(s *report.Step) { s.Concurrency = 2 },
		func(s *report.Step) { s.Method = http.MethodPost },
		func(s *report.Step) { s.Headers = map[string]string{"Host": "example.test"} },
		func(s *report.Step) { s.Body = "content" },
		func(s *report.Step) { s.Expect.Status = 200 },
	} {
		step := report.Step{URL: "http://example.test", Wait: &report.WaitStep{}}
		change(&step)
		result := executeWaitStep(&step)
		if result.OK || result.Err == "" {
			t.Fatalf("invalid wait step accepted: %+v", step)
		}
	}
}
