package statute

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/kjanat/statute/resolved"
)

func TestJSONLog_SampleClamps(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want float64
	}{
		{-1, 0},
		{0, 0},
		{0.5, 0.5},
		{1, 1},
		{2, 1},
	}
	for _, c := range cases {
		j := JSONLog(Stdout).Sample(c.in)
		if j.sampleRate != c.want {
			t.Errorf("Sample(%v): sampleRate=%v, want %v", c.in, j.sampleRate, c.want)
		}
	}
}

func TestOTLP_SampleClamps(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want float64
	}{
		{-1, 0},
		{0.05, 0.05},
		{1, 1},
		{10, 1},
	}
	for _, c := range cases {
		o := OTLP("collector:4317").Sample(c.in)
		if o.sampleRate != c.want {
			t.Errorf("Sample(%v): sampleRate=%v", c.in, o.sampleRate)
		}
	}
}

func TestAccessLog_ShouldLog(t *testing.T) {
	t.Parallel()
	// 5xx and 4xx are always logged regardless of sample rate.
	if !shouldLog(500, 0) {
		t.Errorf("5xx must always log")
	}
	if !shouldLog(404, 0) {
		t.Errorf("4xx must always log")
	}
	// rate=1 logs every 2xx/3xx
	if !shouldLog(200, 1) {
		t.Errorf("2xx with rate=1 must log")
	}
	// rate=0 drops 2xx/3xx
	if shouldLog(200, 0) {
		t.Errorf("2xx with rate=0 must drop")
	}
}

// TestAccessLog_ErrorsAlwaysLogged — set a tiny sample rate and confirm
// 5xx requests still produce log lines.
func TestAccessLog_ErrorsAlwaysLogged(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var mu sync.Mutex
	writer := &mu_writer{Mutex: &mu, w: &buf}

	cfg := resolved.AccessLog{
		Enabled:    true,
		Writer:     writer,
		Format:     "json",
		SampleRate: 0.0001, // ~0% of success
	}
	h := accessLogMiddleware(cfg, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	for range 5 {
		runRequest(t, h, httptest.NewRequest("GET", "/", nil))
	}

	// Every line is a JSON object with status=500.
	mu.Lock()
	out := buf.String()
	mu.Unlock()

	lines := 0
	dec := json.NewDecoder(bytes.NewReader([]byte(out)))
	for dec.More() {
		var v map[string]any
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if v["status"].(float64) != 500 {
			t.Errorf("logged status %v, want 500", v["status"])
		}
		lines++
	}
	if lines != 5 {
		t.Errorf("logged lines: got %d, want 5 (errors must always log)", lines)
	}
}

// TestAccessLog_DisabledIsNoOp
func TestAccessLog_DisabledIsNoOp(t *testing.T) {
	t.Parallel()
	cfg := resolved.AccessLog{Enabled: false}
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := accessLogMiddleware(cfg, inner)
	// Should be the same handler; we can't compare function pointers safely
	// but we can verify the inner runs.
	rec := runRequest(t, h, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatal("inner did not run")
	}
}

// TestStats_PrometheusFormat — ensure the WritePrometheus output is
// parseable text with the canonical counter names.
func TestStats_PrometheusFormat(t *testing.T) {
	t.Parallel()
	s := newStats()
	s.Observe(200, 0)
	s.Observe(200, 0)
	s.Observe(500, 0)

	var buf bytes.Buffer
	s.WritePrometheus(&buf)
	out := buf.String()

	wantSubstrings := []string{
		"# TYPE statute_requests_total counter",
		"statute_requests_total 3",
		`statute_requests_by_status_total{status="200"} 2`,
		`statute_requests_by_status_total{status="500"} 1`,
	}
	for _, w := range wantSubstrings {
		if !bytes.Contains([]byte(out), []byte(w)) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
}

// mu_writer is a tiny synchronized writer used in concurrent encoder tests.
type mu_writer struct {
	*sync.Mutex
	w *bytes.Buffer
}

func (m *mu_writer) Write(p []byte) (int, error) {
	m.Lock()
	defer m.Unlock()
	return m.w.Write(p)
}
