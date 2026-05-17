package statute

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestETag_AddsHeaderOn200(t *testing.T) {
	t.Parallel()
	body := "hello, world\n"
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})
	h := etagHandler(inner)

	rec := runRequest(t, h, httptest.NewRequest("GET", "/", nil))
	if rec.Header().Get("ETag") == "" {
		t.Errorf("ETag header missing")
	}
	if rec.Body.String() != body {
		t.Errorf("body altered")
	}
}

func TestETag_NotModifiedOnMatch(t *testing.T) {
	t.Parallel()
	body := "static asset content"
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})
	h := etagHandler(inner)

	// First request to learn the ETag.
	rec := runRequest(t, h, httptest.NewRequest("GET", "/", nil))
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on first response")
	}

	// Conditional request.
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("If-None-Match", etag)
	rec = runRequest(t, h, req)
	if rec.Code != http.StatusNotModified {
		t.Errorf("status: got %d, want 304", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("304 must have empty body, got %d bytes", rec.Body.Len())
	}
}

func TestETag_DoesNotInterfereWithNon200(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("oops"))
	})
	h := etagHandler(inner)

	rec := runRequest(t, h, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d", rec.Code)
	}
	if rec.Header().Get("ETag") != "" {
		t.Errorf("ETag must not be set on 5xx")
	}
}

func TestETag_SkipsNonGetHead(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("x"))
	})
	h := etagHandler(inner)

	rec := runRequest(t, h, httptest.NewRequest("POST", "/", nil))
	if rec.Header().Get("ETag") != "" {
		t.Errorf("ETag must not be set for POST")
	}
}
