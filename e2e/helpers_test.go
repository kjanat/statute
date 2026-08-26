//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"statute.kjanat.dev/e2e/harness"
)

// clientGet fetches a URL from inside the network through the client
// actor and returns the body.
func clientGet(ctx context.Context, r *harness.Run, url string, extra ...string) (string, error) {
	args := append([]string{"get", "-url", url}, extra...)
	return r.Compose.RunClient(ctx, r.Topology.Clients[0], args...)
}

// mustClientGet is clientGet with failure fatal to the test.
func mustClientGet(ctx context.Context, r *harness.Run, url string, extra ...string) string {
	r.T.Helper()
	out, err := clientGet(ctx, r, url, extra...)
	if err != nil {
		r.T.Fatalf("get %s: %v", url, err)
	}
	return out
}

// journalEntry mirrors the origin actor's journal record — the shared
// contract is the JSON shape, deliberately not a shared Go type, so a
// drift in what the origin reports breaks a test instead of hiding in a
// common struct.
type journalEntry struct {
	Origin    string `json:"origin"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	Query     string `json:"query"`
	Host      string `json:"host"`
	Proto     string `json:"proto"`
	RequestID string `json:"request_id"`
	Forwarded string `json:"x_forwarded_for"`
	SNI       string `json:"sni"`
}

// originJournal reads one origin's request journal; scheme is "http" or
// "https" (TLS origins need the lane roots).
func originJournal(ctx context.Context, r *harness.Run, origin, scheme string) []journalEntry {
	r.T.Helper()
	url := fmt.Sprintf("%s://%s:%d/admin/requests", scheme, origin, harness.PortOrigin)
	extra := []string{}
	if scheme == "https" {
		extra = append(extra, "-roots", "/certs/ca.crt")
	}
	out := mustClientGet(ctx, r, url, extra...)
	var entries []journalEntry
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		r.T.Fatalf("journal of %s: %v\n%s", origin, err, out)
	}
	return entries
}

// setOriginHealth flips one origin's /health endpoint.
func setOriginHealth(ctx context.Context, r *harness.Run, origin, state string) {
	r.T.Helper()
	mustClientGet(ctx, r, fmt.Sprintf("http://%s:%d/admin/health?state=%s", origin, harness.PortOrigin, state))
}

// pollUntil retries fn every 500ms until it reports success or the
// budget runs out, then fails the test with the last observation.
func pollUntil(t *testing.T, budget time.Duration, what string, fn func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(budget)
	var last string
	for time.Now().Before(deadline) {
		ok, obs := fn()
		if ok {
			return
		}
		last = obs
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s: not reached within %s; last: %s", what, budget, last)
}

// awaitOriginPath waits until either origin's journal records a request
// for the given path — proof the request is on the wire, not merely
// planned.
func awaitOriginPath(ctx context.Context, t *testing.T, r *harness.Run, path string) {
	t.Helper()
	pollUntil(t, 30*time.Second, "request for "+path+" reaches an origin", func() (bool, string) {
		for _, origin := range []string{"origin-1", "origin-2"} {
			for _, e := range originJournal(ctx, r, origin, "http") {
				if e.Path == path {
					return true, ""
				}
			}
		}
		return false, "no journal entry yet"
	})
}

// awaitOriginQuery waits until either origin's journal records a request
// whose query carries the marker — proof that this batch of traffic, not
// an earlier one, is already on the wire.
func awaitOriginQuery(ctx context.Context, t *testing.T, r *harness.Run, marker string) {
	t.Helper()
	pollUntil(t, 60*time.Second, "request carrying "+marker+" reaches an origin", func() (bool, string) {
		for _, origin := range []string{"origin-1", "origin-2"} {
			for _, e := range originJournal(ctx, r, origin, "http") {
				if strings.Contains(e.Query, marker) {
					return true, ""
				}
			}
		}
		return false, "no journal entry yet"
	})
}

// logLinesContaining returns the log lines that contain every marker.
func logLinesContaining(logs string, markers ...string) []string {
	var out []string
	for line := range strings.SplitSeq(logs, "\n") {
		all := true
		for _, m := range markers {
			if !strings.Contains(line, m) {
				all = false
				break
			}
		}
		if all {
			out = append(out, line)
		}
	}
	return out
}
