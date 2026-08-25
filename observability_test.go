package statute

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"statute.kjanat.dev/resolved"
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
	if !shouldLog(500, 0, nil) {
		t.Errorf("5xx must always log")
	}
	if !shouldLog(404, 0, nil) {
		t.Errorf("4xx must always log")
	}
	// rate=1 logs every 2xx/3xx
	if !shouldLog(200, 1, nil) {
		t.Errorf("2xx with rate=1 must log")
	}
	// rate=0 drops 2xx/3xx
	if shouldLog(200, 0, nil) {
		t.Errorf("2xx with rate=0 must drop")
	}
}

// TestAccessLog_StatusFilter — the status filter is a hard gate ahead of
// every other rule: outside the ranges nothing logs, not even a 5xx; inside
// the ranges errors bypass sampling and successes still sample.
func TestAccessLog_StatusFilter(t *testing.T) {
	t.Parallel()
	errors := []resolved.StatusRange{{From: 400, To: 599}}
	// Outside every range: never logged, even a 5xx at rate=1.
	if shouldLog(200, 1, errors) {
		t.Errorf("200 outside filter must drop")
	}
	success := []resolved.StatusRange{{From: 200, To: 299}}
	if shouldLog(500, 1, success) {
		t.Errorf("500 outside Statuses(\"200-299\") must drop")
	}
	// Inside a range: errors bypass sampling, successes sample.
	if !shouldLog(503, 0, errors) {
		t.Errorf("5xx inside filter must always log")
	}
	if !shouldLog(204, 1, success) {
		t.Errorf("2xx inside filter with rate=1 must log")
	}
	if shouldLog(204, 0, success) {
		t.Errorf("2xx inside filter with rate=0 must drop")
	}
	// Bounds are inclusive.
	if !shouldLog(400, 0, errors) || !shouldLog(599, 0, errors) {
		t.Errorf("range bounds must be inclusive")
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

// TestAccessLog_StatusFilterSuppressesErrors — Statuses("200-299") really
// does suppress 500s end to end: the filter gates ahead of the
// errors-always-logged rule, while in-range successes still log.
func TestAccessLog_StatusFilterSuppressesErrors(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var mu sync.Mutex
	writer := &mu_writer{Mutex: &mu, w: &buf}

	cfg := resolved.AccessLog{
		Enabled:    true,
		Writer:     writer,
		Format:     "json",
		SampleRate: 1,
		Statuses:   []resolved.StatusRange{{From: 200, To: 299}},
	}
	h := accessLogMiddleware(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/boom" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	for range 3 {
		runRequest(t, h, httptest.NewRequest("GET", "/boom", nil))
	}
	runRequest(t, h, httptest.NewRequest("GET", "/ok", nil))

	mu.Lock()
	out := buf.String()
	mu.Unlock()

	dec := json.NewDecoder(bytes.NewReader([]byte(out)))
	lines := 0
	for dec.More() {
		var v map[string]any
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if v["status"].(float64) != 200 {
			t.Errorf("logged status %v, want only 200 (500s are outside the filter)", v["status"])
		}
		lines++
	}
	if lines != 1 {
		t.Errorf("logged lines: got %d, want 1", lines)
	}
}

// TestAccessLog_StatusFilterFinalStatus — a 103 interim response followed by
// a 404 filters as 404: the recorder ignores 1xx, so an error-range filter
// still logs the exchange.
func TestAccessLog_StatusFilterFinalStatus(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var mu sync.Mutex
	writer := &mu_writer{Mutex: &mu, w: &buf}

	cfg := resolved.AccessLog{
		Enabled:    true,
		Writer:     writer,
		Format:     "json",
		SampleRate: 1,
		Statuses:   []resolved.StatusRange{{From: 400, To: 499}},
	}
	h := accessLogMiddleware(cfg, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusEarlyHints)
		w.WriteHeader(http.StatusNotFound)
	}))
	runRequest(t, h, httptest.NewRequest("GET", "/", nil))

	mu.Lock()
	out := buf.String()
	mu.Unlock()

	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("expected one logged line, got %q: %v", out, err)
	}
	if v["status"].(float64) != 404 {
		t.Errorf("logged status %v, want 404 (final status, not the 103 interim)", v["status"])
	}
}

// TestAccessLog_StatusFilterFlushCommits — a Flush before any WriteHeader
// commits an implicit 200 to the client; a later WriteHeader(500) cannot
// change that, so the filter must see 200, not 500. Statuses("500-599")
// stays silent and Statuses("200-299") logs the request as 200.
func TestAccessLog_StatusFilterFlushCommits(t *testing.T) {
	t.Parallel()
	handler := func(w http.ResponseWriter, _ *http.Request) {
		w.(http.Flusher).Flush() // client is now committed to 200
		w.WriteHeader(http.StatusInternalServerError)
	}

	logged := func(statuses []resolved.StatusRange) string {
		var buf bytes.Buffer
		var mu sync.Mutex
		cfg := resolved.AccessLog{
			Enabled:    true,
			Writer:     &mu_writer{Mutex: &mu, w: &buf},
			Format:     "json",
			SampleRate: 1,
			Statuses:   statuses,
		}
		runRequest(t, accessLogMiddleware(cfg, http.HandlerFunc(handler)), httptest.NewRequest("GET", "/", nil))
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}

	if out := logged([]resolved.StatusRange{{From: 500, To: 599}}); out != "" {
		t.Errorf("Statuses(\"500-599\") logged %q, want nothing: the committed status is 200", out)
	}
	out := logged([]resolved.StatusRange{{From: 200, To: 299}})
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("expected one logged line, got %q: %v", out, err)
	}
	if v["status"].(float64) != 200 {
		t.Errorf("logged status %v, want 200 (flush committed the implicit 200)", v["status"])
	}
}

// TestAccessLog_StatusFilter101 — 101 Switching Protocols is the one 1xx
// net/http records as final: no further response may follow it, so the
// filter must see 101, not the recorder's default 200.
func TestAccessLog_StatusFilter101(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var mu sync.Mutex
	cfg := resolved.AccessLog{
		Enabled:    true,
		Writer:     &mu_writer{Mutex: &mu, w: &buf},
		Format:     "json",
		SampleRate: 1,
		Statuses:   []resolved.StatusRange{{From: 101, To: 101}},
	}
	h := accessLogMiddleware(cfg, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusSwitchingProtocols)
	}))
	runRequest(t, h, httptest.NewRequest("GET", "/", nil))

	mu.Lock()
	out := buf.String()
	mu.Unlock()

	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("expected one logged line, got %q: %v", out, err)
	}
	if v["status"].(float64) != 101 {
		t.Errorf("logged status %v, want 101 (Switching Protocols is final)", v["status"])
	}
}

// TestStatusRecorderHijack — an upgrade must still be able to take the
// connection through the full observability stack: metricsMiddleware wraps
// every listener with the statusRecorder, the access log adds a second one,
// and the reverse proxy hijacks via http.ResponseController, which can only
// reach the underlying writer through Hijacker or Unwrap.
func TestStatusRecorderHijack(t *testing.T) {
	t.Parallel()
	rec := &hijackableRecorder{ResponseRecorder: httptest.NewRecorder()}
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, _, err := http.NewResponseController(w).Hijack(); err != nil {
			t.Errorf("hijack through the recorder: %v", err)
		}
	})
	var buf bytes.Buffer
	var mu sync.Mutex
	cfg := resolved.AccessLog{Enabled: true, Writer: &mu_writer{Mutex: &mu, w: &buf}, Format: "json", SampleRate: 1}
	// Stack both recorders the way server.go does: access log inside, metrics outside.
	h := metricsMiddleware(&stats{}, accessLogMiddleware(cfg, inner))
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	if !rec.hijacked {
		t.Error("hijack did not reach the underlying ResponseWriter")
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
// statusCapturingWriter records every status code forwarded to it — including
// the informational ones httptest.ResponseRecorder would latch on.
type statusCapturingWriter struct {
	http.ResponseWriter
	codes []int
}

func (w *statusCapturingWriter) WriteHeader(code int) { w.codes = append(w.codes, code) }

// TestStatusRecorderInformationalStatus — a 1xx preview passes through
// without latching, so the final status is still forwarded and is what the
// access log and metrics record. Latching on the preview would swallow the
// final WriteHeader; net/http would then commit an implicit 200, turning
// e.g. a 404 behind these middlewares into a success.
func TestStatusRecorderInformationalStatus(t *testing.T) {
	t.Parallel()
	spy := &statusCapturingWriter{ResponseWriter: httptest.NewRecorder()}
	ww := &statusRecorder{ResponseWriter: spy, status: 200}
	ww.WriteHeader(http.StatusEarlyHints)
	ww.WriteHeader(http.StatusNotFound)
	// A duplicate final status stays swallowed, shielding the underlying
	// writer from a superfluous WriteHeader.
	ww.WriteHeader(http.StatusInternalServerError)

	if want := []int{http.StatusEarlyHints, http.StatusNotFound}; !slices.Equal(spy.codes, want) {
		t.Errorf("forwarded codes: got %v, want %v", spy.codes, want)
	}
	if ww.status != http.StatusNotFound {
		t.Errorf("recorded status: got %d, want %d", ww.status, http.StatusNotFound)
	}
}

// TestStatusRecorderReadFrom — the recorder implements io.ReaderFrom so a
// body copy keeps net/http's sendfile path, and an un-preceded copy still
// counts as the implicit 200.
func TestStatusRecorderReadFrom(t *testing.T) {
	t.Parallel()
	rec := &readerFromRecorder{ResponseRecorder: httptest.NewRecorder()}
	ww := &statusRecorder{ResponseWriter: rec, status: 200}
	if _, err := io.Copy(ww, plainReader{strings.NewReader("payload")}); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if !rec.readFrom {
		t.Error("copy did not reach the underlying ReadFrom")
	}
	if !ww.wroteHeader || ww.status != 200 {
		t.Errorf("recorder after copy: wroteHeader=%v status=%d, want implicit 200", ww.wroteHeader, ww.status)
	}
	if got := rec.Body.String(); got != "payload" {
		t.Errorf("body: got %q", got)
	}

	// Without a ReaderFrom underneath, the copy falls back to a plain write.
	plain := httptest.NewRecorder()
	ww = &statusRecorder{ResponseWriter: plain, status: 200}
	if _, err := io.Copy(ww, plainReader{strings.NewReader("payload")}); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if got := plain.Body.String(); got != "payload" {
		t.Errorf("fallback body: got %q", got)
	}
}

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
