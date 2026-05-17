package statute

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kjanat/statute/resolved"
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

func TestParseSize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want int64
		err  bool
	}{
		{"1", 1, false},
		{"100", 100, false},
		{"1KB", 1000, false},
		{"1KiB", 1024, false},
		{"2MB", 2_000_000, false},
		{"2MiB", 2 * 1024 * 1024, false},
		{"1.5GB", 1_500_000_000, false},
		{"512B", 512, false},
		{"  10kb  ", 10_000, false},
		{"abc", 0, true},
		{"-1MB", 0, true},
		{"1XB", 0, true},
		{"", 0, true},
	}
	for _, c := range cases {
		got, err := parseSize(c.in)
		if c.err {
			if err == nil {
				t.Errorf("parseSize(%q) = %d, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSize(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}
