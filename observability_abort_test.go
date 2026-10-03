package statute

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"statute.kjanat.dev/resolved"
)

func TestObservabilityAbortedResponse(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		abort      bool
		wantStatus int
	}{
		{"before headers", 0, "", true, 0},
		{"after informational", 103, "", true, 0},
		{"after prefix", 200, "prefix", true, 200},
		{"implicit prefix", 0, "prefix", true, 200},
		{"normal empty", 0, "", false, 200},
		{"normal body", 202, "body", false, 202},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var log bytes.Buffer
			s := newStats()
			cfg := resolved.AccessLog{Enabled: true, Writer: &log, SampleRate: 1}
			handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				if tc.body != "" {
					_, _ = io.WriteString(w, tc.body)
				}
				if tc.abort {
					panic(http.ErrAbortHandler)
				}
			})
			h := metricsMiddleware(s, accessLogMiddleware(cfg, handler))
			got := handlerPanic(h, httptest.NewRecorder())
			var wantPanic any
			if tc.abort {
				wantPanic = http.ErrAbortHandler
			}
			if got != wantPanic {
				t.Fatalf("panic changed: %v", got)
			}
			checkObservedResponse(t, &log, s, tc.wantStatus, len(tc.body), tc.abort, false)
		})
	}
}

func handlerPanic(h http.Handler, w http.ResponseWriter) (value any) {
	defer func() { value = recover() }()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/stream", nil))
	return nil
}

func checkObservedResponse(t *testing.T, log *bytes.Buffer, s *stats, status, size int, aborted, bodyError bool) {
	t.Helper()
	var entry map[string]any
	dec := json.NewDecoder(log)
	if err := dec.Decode(&entry); err != nil {
		t.Fatal(err)
	}
	if entry["status"] != float64(status) || entry["body_bytes"] != float64(size) || entry["aborted"] != aborted || entry["body_error"] != bodyError {
		t.Fatalf("incorrect observation: %v", entry)
	}
	if err := dec.Decode(&entry); err != io.EOF {
		t.Fatalf("expected exactly one log record: %v", err)
	}
	checkObservedMetrics(t, s, status, size, aborted, bodyError)
}

func checkObservedMetrics(t *testing.T, s *stats, status, size int, aborted, bodyError bool) {
	t.Helper()
	if s.requests.Load() != 1 || s.durationCount.Load() != 1 || s.responseBytes.Load() != uint64(max(size, 0)) {
		t.Fatal("missing or duplicated request/byte metrics")
	}
	count, ok := s.requestsByStatus.Load(status)
	if !ok || count.(*atomic.Uint64).Load() != 1 {
		t.Fatal("incorrect final-status metric")
	}
	if (s.aborted.Load() == 1) != aborted || (s.bodyErrors.Load() == 1) != bodyError {
		t.Fatal("incorrect failure metrics")
	}
}

func TestAbortedAccessLogFiltering(t *testing.T) {
	for _, tc := range []struct {
		name     string
		statuses []resolved.StatusRange
		wantLog  bool
	}{
		{"unfiltered", nil, true},
		{"matching", []resolved.StatusRange{{From: 200, To: 299}}, true},
		{"excluded", []resolved.StatusRange{{From: 400, To: 599}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var log bytes.Buffer
			cfg := resolved.AccessLog{Enabled: true, Writer: &log, SampleRate: 1e-10, Statuses: tc.statuses}
			h := accessLogMiddleware(cfg, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(200)
				panic("sensitive panic detail")
			}))
			if got := handlerPanic(h, httptest.NewRecorder()); got != "sensitive panic detail" {
				t.Fatalf("panic not preserved: %v", got)
			}
			if (log.Len() != 0) != tc.wantLog || strings.Contains(log.String(), "sensitive panic detail") {
				t.Fatalf("filtering or panic disclosure: %s", log.String())
			}
		})
	}
}

type partialErrorWriter struct {
	*httptest.ResponseRecorder
	copyCalled bool
}

func (*partialErrorWriter) Write([]byte) (int, error) {
	return 2, io.ErrClosedPipe
}

func (w *partialErrorWriter) ReadFrom(io.Reader) (int64, error) {
	w.copyCalled = true
	return 2, io.ErrClosedPipe
}

func TestObservabilityBodyIOError(t *testing.T) {
	for _, copyBody := range []bool{false, true} {
		var log bytes.Buffer
		s := newStats()
		cfg := resolved.AccessLog{Enabled: true, Writer: &log, SampleRate: 1e-10}
		h := accessLogMiddleware(cfg, metricsMiddleware(s, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			var err error
			if copyBody {
				_, err = w.(io.ReaderFrom).ReadFrom(strings.NewReader("body"))
			} else {
				_, err = w.Write([]byte("body"))
			}
			if !errors.Is(err, io.ErrClosedPipe) {
				t.Errorf("I/O error lost: %v", err)
			}
		})))
		w := &partialErrorWriter{ResponseRecorder: httptest.NewRecorder()}
		h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
		if w.copyCalled != copyBody {
			t.Fatal("ReaderFrom delegation changed")
		}
		checkObservedResponse(t, &log, s, 200, 2, false, true)
	}
}
