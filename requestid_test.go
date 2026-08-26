package statute

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"statute.kjanat.dev/resolved"
)

func TestRequestID_GeneratedWhenAbsent(t *testing.T) {
	t.Parallel()
	var upstreamSeen string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamSeen = r.Header.Get("X-Request-Id")
		w.WriteHeader(http.StatusOK)
	})
	h := requestIDHandler(resolved.Middleware{Type: resolved.MWRequestID, RequestIDHeader: "X-Request-Id"}, inner)
	rec := runRequest(t, h, httptest.NewRequest("GET", "/", nil))

	respID := rec.Header().Get("X-Request-Id")
	if respID == "" {
		t.Fatal("no X-Request-Id on response")
	}
	if upstreamSeen != respID {
		t.Errorf("upstream saw %q, response had %q — must match", upstreamSeen, respID)
	}
	if len(respID) != 32 {
		t.Errorf("ID length: got %d, want 32 (16 bytes hex)", len(respID))
	}
}

func TestRequestID_AcceptsInboundFromHeader(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {})
	h := requestIDHandler(resolved.Middleware{
		Type:                resolved.MWRequestID,
		RequestIDHeader:     "X-Request-Id",
		RequestIDFromHeader: "X-Trace-Id",
	}, inner)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Trace-Id", "inbound-trace-123")
	rec := runRequest(t, h, req)

	if got := rec.Header().Get("X-Request-Id"); got != "inbound-trace-123" {
		t.Errorf("got %q, want inbound value", got)
	}
}

func TestRequestID_PropagatesToAccessLog(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	cfg := resolved.AccessLog{Enabled: true, Format: "json", Writer: &buf, SampleRate: 1.0}

	chain := requestIDHandler(
		resolved.Middleware{Type: resolved.MWRequestID, RequestIDHeader: "X-Request-Id"},
		accessLogMiddleware(cfg, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})),
	)
	rec := runRequest(t, chain, httptest.NewRequest("GET", "/", nil))
	expected := rec.Header().Get("X-Request-Id")
	if expected == "" {
		t.Fatal("middleware did not set X-Request-Id")
	}

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("log not JSON: %v\n%s", err, buf.String())
	}
	if entry["request_id"] != expected {
		t.Errorf("log request_id = %v, want %q", entry["request_id"], expected)
	}
}

func TestRequestID_AbsentFromLogWhenMiddlewareNotApplied(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	cfg := resolved.AccessLog{Enabled: true, Format: "json", Writer: &buf, SampleRate: 1.0}
	h := accessLogMiddleware(cfg, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	runRequest(t, h, httptest.NewRequest("GET", "/", nil))

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if _, ok := entry["request_id"]; ok {
		t.Errorf("request_id present without middleware: %v", entry["request_id"])
	}
}

// TestRequestID_PropagatesToListenerLevelAccessLog proves the id
// reaches the log in the runtime's real composition: the access log
// wraps the routed content path from the listener, OUTSIDE the route
// middleware chain, so the value must travel upward through the
// installed holder rather than a derived downstream context.
func TestRequestID_PropagatesToListenerLevelAccessLog(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	cfg := resolved.AccessLog{Enabled: true, Format: "json", Writer: &buf, SampleRate: 1.0}

	chain := accessLogMiddleware(cfg, requestIDHandler(
		resolved.Middleware{Type: resolved.MWRequestID, RequestIDHeader: "X-Request-Id"},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	))
	rec := runRequest(t, chain, httptest.NewRequest("GET", "/", nil))
	expected := rec.Header().Get("X-Request-Id")
	if expected == "" {
		t.Fatal("middleware did not set X-Request-Id")
	}

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("log not JSON: %v\n%s", err, buf.String())
	}
	if entry["request_id"] != expected {
		t.Errorf("log request_id = %v, want %q", entry["request_id"], expected)
	}
}
