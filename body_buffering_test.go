package statute

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"statute.kjanat.dev/resolved"
)

func TestBodyBufferSpillsAndReplaysRequests(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	body := strings.Repeat("request", 32)
	calls := 0
	h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		got, err := io.ReadAll(r.Body)
		if err != nil || string(got) != body {
			t.Errorf("attempt %d body differs: %v", calls, err)
		}
		w.Header().Set("Content-Type", "text/plain")
		if calls == 1 {
			w.WriteHeader(http.StatusBadGateway)
		}
		_, _ = io.WriteString(w, body)
	}), BufferBodies(BodyBufferPolicy{MaxRequestBytes: 1024, MemoryRequestBytes: 8, MaxResponseBytes: 1024, MemoryResponseBytes: 8, RetryExpression: "Attempts() < 2 && ResponseCode() == 502"}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
	if calls != 2 || w.Code != http.StatusOK || w.Body.String() != body {
		t.Fatalf("calls=%d status=%d body=%q", calls, w.Code, w.Body.String())
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("spill files leaked: %v %v", entries, err)
	}
}

func TestBodyBufferLimitsAndDisabledDirections(t *testing.T) {
	for _, tc := range []struct {
		policy BodyBufferPolicy
		status int
	}{
		{BodyBufferPolicy{DisableRequest: true, DisableResponse: true}, 200},
		{BodyBufferPolicy{MaxRequestBytes: 4}, 413},
		{BodyBufferPolicy{MaxResponseBytes: 4}, 500},
	} {
		h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = io.WriteString(w, "response")
		}), BufferBodies(tc.policy))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("request")))
		if w.Code != tc.status {
			t.Fatalf("buffer limit status=%d, want %d", w.Code, tc.status)
		}
		if tc.status == 200 && w.Body.String() != "response" {
			t.Fatal("disabled buffering changed response")
		}
	}
}

func TestBodyBufferInvalidPolicyRefuses(t *testing.T) {
	if _, err := resolveMiddleware(BufferBodies(BodyBufferPolicy{RetryExpression: "invalid("})); err == nil {
		t.Fatal("invalid retry expression accepted")
	}
	for _, m := range []resolved.Middleware{{}, {BodyBuffers: &BodyBufferPolicy{RetryExpression: "invalid("}}} {
		w := httptest.NewRecorder()
		bufferBodiesHandler(m, http.NotFoundHandler()).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
		if w.Code != http.StatusInternalServerError {
			t.Fatal(w.Code)
		}
	}
}
