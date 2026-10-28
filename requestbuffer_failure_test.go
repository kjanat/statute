package statute

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestRequestBufferRetiredFactoryCannotReopen(t *testing.T) {
	for _, disk := range []bool{false, true} {
		t.Run(fmtBufferCase(disk, 3, false), func(t *testing.T) {
			m := bufferTestManager(t, disk, 16, 5)
			var factory func() (io.ReadCloser, error)
			h := bufferTestHandler(t, m, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { factory = r.GetBody }), RequestBuffer("5B").MemoryThreshold("2B"))
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/", strings.NewReader("abc")))
			assertBufferReleased(t, m)
			reader, err := factory()
			if reader != nil || !errors.Is(err, http.ErrBodyReadAfterClose) {
				t.Fatalf("GetBody after buffer release returned %v, %v", reader, err)
			}
			assertBufferReleased(t, m)
		})
	}
}

func TestRequestBufferMissingManagerFailsClosed(t *testing.T) {
	policy, err := resolveMiddleware(RequestBuffer("5B"))
	if err != nil {
		t.Fatal(err)
	}
	h := requestBufferHandler(policy, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("missing resources invoked downstream") }))
	body := &requestBufferFaultBody{read: func([]byte) (int, error) { t.Error("body read before memory was reserved"); return 0, io.EOF }}
	req := httptest.NewRequest("POST", "/", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("response=%v", rec)
	}
}

func TestRequestBufferBodylessRequests(t *testing.T) {
	for _, body := range []io.ReadCloser{nil, http.NoBody} {
		m := bufferTestManager(t, false, 6, 0)
		calls := 0
		h := bufferTestHandler(t, m, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			calls++
			assertRequestBufferReplay(t, r, "")
		}), RequestBuffer("5B"))
		req := httptest.NewRequest("GET", "/", nil)
		req.Body = body
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 || calls != 1 {
			t.Fatalf("status=%d calls=%d", rec.Code, calls)
		}
		assertBufferReleased(t, m)
	}
}

type requestBufferFaultBody struct {
	read          func([]byte) (int, error)
	reads, closes int
}

func (b *requestBufferFaultBody) Read(p []byte) (int, error) { b.reads++; return b.read(p) }
func (b *requestBufferFaultBody) Close() error               { b.closes++; return nil }

func TestRequestBufferCancellationAtReadBoundaries(t *testing.T) {
	for _, eof := range []bool{false, true} {
		t.Run(fmtBufferCase(false, 1, eof), func(t *testing.T) {
			m := bufferTestManager(t, false, 6, 0)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			body := &requestBufferFaultBody{read: func(p []byte) (int, error) {
				p[0] = 'x'
				cancel()
				if eof {
					return 1, io.EOF
				}
				return 1, nil
			}}
			h := bufferTestHandler(t, m, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("cancelled request invoked downstream") }), RequestBuffer("5B"))
			req := httptest.NewRequest("POST", "/", body).WithContext(ctx)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != 503 || body.reads != 1 || body.closes != 1 {
				t.Fatalf("status=%d reads=%d closes=%d", rec.Code, body.reads, body.closes)
			}
			assertBufferReleased(t, m)
		})
	}
}

func TestRequestBufferNonProgressingReader(t *testing.T) {
	m := bufferTestManager(t, false, 6, 0)
	body := &requestBufferFaultBody{read: func([]byte) (int, error) { return 0, nil }}
	h := bufferTestHandler(t, m, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("incomplete request invoked downstream") }), RequestBuffer("5B"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/", body))
	if rec.Code != 400 || body.reads != 100 || body.closes != 1 {
		t.Fatalf("status=%d reads=%d closes=%d", rec.Code, body.reads, body.closes)
	}
	assertBufferReleased(t, m)
}

func TestRequestBufferPreservesOuterBodyLimitFailure(t *testing.T) {
	m := bufferTestManager(t, false, 6, 0)
	h := bufferTestHandler(t, m, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("BodyLimit failure invoked downstream") }), BodyLimit("2B"), RequestBuffer("5B"))
	req := httptest.NewRequest("POST", "/", strings.NewReader("abc"))
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 413 {
		t.Fatalf("status=%d", rec.Code)
	}
	assertBufferReleased(t, m)
}

func TestRequestBufferUnlinkFailureCleansSpool(t *testing.T) {
	m := bufferTestManager(t, true, 3, 5)
	attempts := 0
	m.removeSpool = func(path string) error {
		attempts++
		if attempts == 1 {
			return &os.PathError{Op: "remove", Path: path, Err: os.ErrPermission}
		}
		return os.Remove(path)
	}
	h := bufferTestHandler(t, m, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("unlink failure invoked downstream") }), RequestBuffer("5B").MemoryThreshold("2B"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/", strings.NewReader("abc")))
	if rec.Code != 503 || rec.Header().Get("Cache-Control") != "no-store" || attempts != 2 {
		t.Fatalf("status=%d attempts=%d headers=%v", rec.Code, attempts, rec.Header())
	}
	assertBufferReleased(t, m)
}

func TestRequestBufferSpoolWriteFailureRetiresCharge(t *testing.T) {
	m := bufferTestManager(t, true, 3, 5)
	b := m.acquire(3)
	if b == nil {
		t.Fatal("buffer allocation failed")
	}
	copy(b.data, "abc")
	b.size = 3
	if err := b.spill(5); err != nil {
		b.release()
		t.Fatal(err)
	}
	if err := b.file.Close(); err != nil {
		b.release()
		t.Fatal(err)
	}
	// A file write error must reject the request and keep disk space reserved
	// until the buffer is released.
	if status := b.storeChunk([]byte("d"), 5, 2); status != 503 {
		t.Errorf("status=%d", status)
	}
	m.mu.Lock()
	disk := m.disk
	m.mu.Unlock()
	if disk != 5 {
		t.Errorf("disk reservation released early: %d", disk)
	}
	b.release()
	assertBufferReleased(t, m)
}
