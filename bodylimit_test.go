package statute

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"statute.kjanat.dev/resolved"
)

func TestBodyLimit_AllowsWithinLimit(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		_, _ = w.Write(b)
	})
	h := bodyLimitHandler(resolved.Middleware{Type: resolved.MWBodyLimit, BodyLimitBytes: 1024}, inner)

	req := httptest.NewRequest("POST", "/", strings.NewReader("small body"))
	rec := runRequest(t, h, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status: %d", rec.Code)
	}
	if rec.Body.String() != "small body" {
		t.Errorf("body: %q", rec.Body.String())
	}
}

func TestBodyLimit_RejectsOverLimit(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if err != nil {
			// MaxBytesReader sets a *MaxBytesError; the canonical handler
			// translates that to 413.
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	h := bodyLimitHandler(resolved.Middleware{Type: resolved.MWBodyLimit, BodyLimitBytes: 10}, inner)

	req := httptest.NewRequest("POST", "/", strings.NewReader(strings.Repeat("a", 100)))
	rec := runRequest(t, h, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status: got %d, want 413", rec.Code)
	}
}

func TestBodyLimit_ZeroLimitIsNoOp(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// Sanity: the middleware must not wrap when the resolved limit is zero.
	h := bodyLimitHandler(resolved.Middleware{Type: resolved.MWBodyLimit, BodyLimitBytes: 0}, inner)
	rec := runRequest(t, h, httptest.NewRequest("POST", "/", strings.NewReader("anything")))
	if rec.Code != http.StatusOK {
		t.Errorf("zero limit must pass through: %d", rec.Code)
	}
}
