package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOriginPreservesBytesInSafeEnvelope(t *testing.T) {
	t.Parallel()
	for _, payload := range [][]byte{nil, []byte("first cold request"), {0, 0xff, 0xfe, '\n'}, []byte(`<script>alert("reflected")</script>`)} {
		req := httptest.NewRequest(http.MethodPost, "/upload", bytes.NewReader(payload))
		rw := httptest.NewRecorder()
		originServer().Handler.ServeHTTP(rw, req)
		if rw.Code != http.StatusOK || rw.Header().Get("Content-Type") != "application/json" || rw.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("unsafe response: status=%d headers=%v", rw.Code, rw.Header())
		}
		var got struct {
			Body []byte `json:"body"`
		}
		if err := json.Unmarshal(rw.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got.Body, payload) || strings.Contains(rw.Body.String(), "<script>") {
			t.Fatalf("echo changed payload or reflected raw HTML: %q", rw.Body.String())
		}
	}
}

func TestOriginHealthAndTimeouts(t *testing.T) {
	t.Parallel()
	srv := originServer()
	if srv.Addr != ":7000" || srv.ReadHeaderTimeout != 5*time.Second || srv.ReadTimeout != 10*time.Second || srv.WriteTimeout != 10*time.Second {
		t.Fatalf("unexpected origin server: %+v", srv)
	}
	rw := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rw.Code != http.StatusOK || rw.Body.String() != "ready\n" {
		t.Fatalf("health = %d %q", rw.Code, rw.Body.String())
	}
}

type failedReader struct{}

func (failedReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestOriginRejectsUnreadableBody(t *testing.T) {
	t.Parallel()
	for _, reader := range []io.Reader{failedReader{}, strings.NewReader(strings.Repeat("x", (1<<20)+1))} {
		rw := httptest.NewRecorder()
		originServer().Handler.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, "/upload", reader))
		if rw.Code != http.StatusBadRequest || rw.Body.String() != "cannot read request body\n" {
			t.Fatalf("read failure = %d %q", rw.Code, rw.Body.String())
		}
	}
}

type failedWriter struct {
	header http.Header
	writes int
}

func (w *failedWriter) Header() http.Header { return w.header }
func (*failedWriter) WriteHeader(int)       {}
func (w *failedWriter) Write([]byte) (int, error) {
	w.writes++
	return 0, io.ErrClosedPipe
}

func TestOriginHandlesClosedResponse(t *testing.T) {
	t.Parallel()
	w := &failedWriter{header: make(http.Header)}
	originServer().Handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader("body")))
	if w.writes != 1 {
		t.Fatalf("write attempts = %d, want 1", w.writes)
	}
}
