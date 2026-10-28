//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"

	"statute.kjanat.dev/e2e/harness"
	"statute.kjanat.dev/e2e/report"
)

// Rejected uploads must never reach the upstream. Check the request journal
// collected from the separate upstream process.
func TestRegression_RequestBuffer(t *testing.T) {
	t.Parallel()
	r := harness.Start(t, "request-buffer", harness.MustTopology(t, "1s1c"))
	ctx := context.Background()
	r.AwaitReady(ctx)
	plan := &report.Plan{Name: "request-buffer"}
	for _, proto := range []string{"h1", "h2"} {
		base := "http://statute-1:8080"
		if proto == "h2" {
			base = "https://statute-1:8443"
		}
		for _, tc := range []struct {
			path, body string
			status     int
		}{
			{"/limited/echo?accepted", "12345678", 200},
			{"/small/echo?accepted-small", "x", 200},
			{"/limited/echo?rejected-size", "123456789", 413},
			{"/unavailable/echo?rejected-budget", "123", 503},
			{"/echo?accepted-fallback", "12345678", 200},
		} {
			plan.Steps = append(plan.Steps, report.Step{Name: proto + tc.path, URL: base + tc.path, TargetServer: harness.Server1, Proto: proto, Method: "POST", Body: tc.body, Count: 1, RootsFile: "/certs/ca.crt", Expect: report.Expect{Status: tc.status}})
		}
	}
	r.ExecutePlan(ctx, harness.Client1, plan)
	entries := originJournal(ctx, r, "origin-1", "http")
	accepted := 0
	for _, entry := range entries {
		if strings.Contains(entry.Query, "rejected") {
			t.Errorf("rejected request reached origin: %s", entry.Query)
		}
		if strings.Contains(entry.Query, "accepted") {
			accepted++
		}
	}
	if accepted != 6 {
		t.Fatalf("accepted origin requests=%d want=6", accepted)
	}
}
