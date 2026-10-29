//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"

	"statute.kjanat.dev/e2e/harness"
	"statute.kjanat.dev/e2e/report"
)

func TestRegression_IngressBeforeRoutingAndPolicy(t *testing.T) {
	t.Parallel()
	r := harness.Start(t, "ingress", harness.MustTopology(t, "1s1c"))
	ctx := context.Background()
	r.AwaitReady(ctx)
	steps := []report.Step{
		{Name: "normalize", URL: "http://statute-1:8080/unused/../api/%65cho?x=a%2Fb", Headers: map[string]string{"X-Forwarded-For": "127.0.0.1", "X-Real-IP": "127.0.0.1"}, Expect: report.Expect{Status: 200, BodyContains: `"path":"/echo"`}},
		{Name: "deny", URL: "http://statute-1:8080/private", Headers: map[string]string{"X-Forwarded-For": "127.0.0.1"}, Expect: report.Expect{Status: 403}},
		{Name: "aliases", URL: "http://statute-1:8080/api/echo", Headers: map[string]string{"X_Forwarded_For": "127.0.0.1"}, Expect: report.Expect{Status: 400}},
		{Name: "first-key", URL: "http://statute-1:8080/limited", Headers: map[string]string{"X-Forwarded-For": "192.0.2.1"}, Expect: report.Expect{Status: 200}},
		{Name: "forged-key", URL: "http://statute-1:8080/limited", Headers: map[string]string{"X-Forwarded-For": "192.0.2.2"}, Expect: report.Expect{Status: 429}},
	}
	for i := range steps {
		steps[i].TargetServer, steps[i].Proto, steps[i].Count = harness.Server1, "h1", 1
	}
	r.ExecutePlan(ctx, harness.Client1, &report.Plan{Name: "ingress", Steps: steps})
	entries := originJournal(ctx, r, "origin-1", "http")
	if len(entries) != 2 {
		t.Fatalf("origin requests=%d, want only normalized and admitted requests", len(entries))
	}
	if entries[0].Path != "/echo" || entries[0].Query != "x=a%2Fb" || strings.Contains(entries[0].Forwarded, "127.0.0.1") {
		t.Fatalf("origin received unsanitized request: %+v", entries[0])
	}
}
