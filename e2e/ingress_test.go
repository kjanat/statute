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

func TestRegression_IngressTrailersAndForwardedOperations(t *testing.T) {
	t.Parallel()
	r := harness.Start(t, "ingress", harness.MustTopology(t, "1s1c"))
	ctx := context.Background()
	r.AwaitReady(ctx)
	r.ExecutePlan(ctx, harness.Client1, &report.Plan{Name: "ingress-replay", Steps: []report.Step{
		{
			Name: "forwarded", URL: "http://statute-1:8080/forwarded", TargetServer: harness.Server1, Proto: "h1", Count: 1,
			Headers: map[string]string{"X-Forwarded-For": "203.0.113.5"}, Expect: report.Expect{Status: 200},
		},
		{
			Name: "trailers-retry", URL: "http://statute-1:8080/unused/../upload/%66ail?key=ingress&n=1&trailer=X-Final&trailerValue=complete", TargetServer: harness.Server1, Proto: "h1", Count: 1,
			Method: "PUT", Body: "pt", Trailers: map[string]string{"X-Final": "complete"}, Expect: report.Expect{Status: 200, BodyContains: `"path":"/fail"`},
		},
	}})
	entries := originJournal(ctx, r, "origin-1", "http")
	if len(entries) != 3 {
		t.Fatalf("origin requests=%d, want one forwarded request and two upload attempts", len(entries))
	}
	chain := entries[0].Forwarded
	if strings.Count(chain, "198.51.100.7") != 1 || strings.Contains(chain, "203.0.113.5") || !strings.HasSuffix(chain, ", 198.51.100.7") {
		t.Fatalf("forwarded chain=%q", chain)
	}
	if entries[1].Path != "/fail" || entries[2].Path != "/fail" {
		t.Fatalf("retry paths=%q,%q", entries[1].Path, entries[2].Path)
	}
}
