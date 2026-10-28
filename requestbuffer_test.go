package statute

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"statute.kjanat.dev/resolved"
)

func bufferTestManager(t *testing.T, disk bool, memory, capacity int64) *requestBufferManager {
	t.Helper()
	c := resolved.RequestBufferResources{MemoryBudgetBytes: memory, DiskBudgetBytes: capacity}
	if disk {
		c.Storage = t.TempDir()
	}
	m := newRequestBufferManager(c)
	t.Cleanup(m.cancel)
	return m
}

func bufferTestHandler(t *testing.T, manager *requestBufferManager, next http.Handler, mws ...Middleware) http.Handler {
	t.Helper()
	chain, err := resolveMiddlewares(mws)
	if err != nil {
		t.Fatal(err)
	}
	return manager.wrap(wrapMiddleware(chain, next))
}

func assertBufferReleased(t *testing.T, m *requestBufferManager) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.memory != 0 || m.disk != 0 || m.active != 0 {
		t.Fatalf("resources still owned: memory=%d disk=%d active=%d", m.memory, m.disk, m.active)
	}
	if m.cfg.Storage != "" {
		entries, err := os.ReadDir(m.cfg.Storage)
		if err != nil || len(entries) != 0 {
			t.Fatalf("temporary files: %v, %v", entries, err)
		}
	}
}

func TestRequestBufferAdmissionAndReplay(t *testing.T) {
	for _, disk := range []bool{false, true} {
		for _, size := range []int{0, 2, 3, 5, 6} {
			for _, unknown := range []bool{false, true} {
				t.Run(fmtBufferCase(disk, size, unknown), func(t *testing.T) { checkRequestBufferReplay(t, disk, size, unknown) })
			}
		}
	}
}

func checkRequestBufferReplay(t *testing.T, disk bool, size int, unknown bool) {
	t.Helper()
	m := bufferTestManager(t, disk, 32, 10)
	calls := 0
	h := bufferTestHandler(t, m, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		assertRequestBufferReplay(t, r, strings.Repeat("x", size))
		w.WriteHeader(201)
	}), RequestBuffer("5B").MemoryThreshold("2B"))
	req := httptest.NewRequest("POST", "/", strings.NewReader(strings.Repeat("x", size)))
	if unknown {
		req.ContentLength = -1
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	want, count := 201, 1
	if size > 5 {
		want, count = 413, 0
	}
	if rec.Code != want || calls != count {
		t.Fatalf("status=%d calls=%d", rec.Code, calls)
	}
	assertBufferReleased(t, m)
}

func assertRequestBufferReplay(t *testing.T, r *http.Request, want string) {
	t.Helper()
	data, err := io.ReadAll(r.Body)
	if err != nil || string(data) != want {
		t.Errorf("body=%q error=%v", data, err)
	}
	replay, err := r.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	again, err := io.ReadAll(replay)
	_ = replay.Close()
	if err != nil || !bytes.Equal(data, again) {
		t.Errorf("replay=%q error=%v", again, err)
	}
}

func fmtBufferCase(disk bool, size int, unknown bool) string {
	return fmt.Sprintf("disk=%t/size=%d/unknown=%t", disk, size, unknown)
}

func TestRequestBufferAggregateAndRetiredReaders(t *testing.T) {
	m := bufferTestManager(t, true, 6, 5)
	var retained io.ReadCloser
	h := bufferTestHandler(t, m, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		retained, err = r.GetBody()
		if err != nil {
			t.Fatal(err)
		}
	}), RequestBuffer("5B").MemoryThreshold("2B"))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/", strings.NewReader("12345")))
	sibling := bufferTestHandler(t, m, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("exhausted disk reached sibling") }), RequestBuffer("5B").MemoryThreshold("2B"))
	rec := httptest.NewRecorder()
	sibling.ServeHTTP(rec, httptest.NewRequest("POST", "/", strings.NewReader("abc")))
	if rec.Code != 503 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("response=%v", rec)
	}
	m.stopAdmission()
	m.cancel()
	data, err := io.ReadAll(retained)
	if err != nil || string(data) != "12345" {
		t.Fatalf("retired reader=%q %v", data, err)
	}
	_ = retained.Close()
	_ = retained.Close()
	assertBufferReleased(t, m)
}

func TestRequestBufferTimeoutKeepsLease(t *testing.T) {
	m := bufferTestManager(t, true, 3, 5)
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	h := bufferTestHandler(t, m, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		close(entered)
		<-release
		data, err := io.ReadAll(r.Body)
		if err != nil || string(data) != "12345" {
			t.Errorf("late body=%q %v", data, err)
		}
	}), RequestBuffer("5B").MemoryThreshold("2B"), Timeout("20ms"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/", strings.NewReader("12345")))
	<-entered
	if rec.Code != 503 {
		t.Fatalf("timeout status=%d", rec.Code)
	}
	m.mu.Lock()
	active := m.active
	m.mu.Unlock()
	if active != 1 {
		t.Fatal("late producer lost its lease")
	}
	close(release)
	<-finished
	deadline := time.Now().Add(time.Second)
	for {
		m.mu.Lock()
		active = m.active
		m.mu.Unlock()
		if active == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("late producer leaked lease")
		}
		time.Sleep(time.Millisecond)
	}
	assertBufferReleased(t, m)
}

type strictTrailerReader struct {
	request *http.Request
	done    bool
}

func (b *strictTrailerReader) Read(p []byte) (int, error) {
	if b.done {
		return 0, io.EOF
	}
	b.done = true
	b.request.Trailer = http.Header{"X-Final": {"captured"}}
	return copy(p, "abc"), io.EOF
}
func (*strictTrailerReader) Close() error { return nil }

func TestRequestBufferTrailersRetryAndPanic(t *testing.T) {
	for _, disk := range []bool{false, true} {
		m := bufferTestManager(t, disk, 32, 10)
		attempts := 0
		h := bufferTestHandler(t, m, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts++
			data, err := io.ReadAll(r.Body)
			if err != nil || string(data) != "abc" || r.Trailer.Get("X-Final") != "captured" {
				t.Errorf("body=%q trailers=%v error=%v", data, r.Trailer, err)
			}
			w.WriteHeader(503)
		}), RequestBuffer("5B").MemoryThreshold("2B"), Retry(2, OnStatus(503)))
		req := httptest.NewRequest("PUT", "/", nil)
		req.ProtoMajor = 3
		req.Body = &strictTrailerReader{request: req}
		req.ContentLength = -1
		h.ServeHTTP(httptest.NewRecorder(), req)
		if attempts != 2 {
			t.Fatalf("attempts=%d", attempts)
		}
		assertBufferReleased(t, m)
		panicHandler := bufferTestHandler(t, m, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("producer") }), RequestBuffer("5B").MemoryThreshold("2B"))
		func() {
			defer func() {
				if recover() != "producer" {
					t.Error("panic not preserved")
				}
			}()
			panicHandler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/", strings.NewReader("abc")))
		}()
		assertBufferReleased(t, m)
	}
}

func TestRequestBufferFailureAndConcurrentBudget(t *testing.T) {
	m := bufferTestManager(t, false, 12, 0)
	var wg sync.WaitGroup
	buffers := make(chan *requestBodyBuffer, 32)
	for range 32 {
		wg.Go(func() {
			if b := m.acquire(6); b != nil {
				buffers <- b
			}
		})
	}
	wg.Wait()
	close(buffers)
	count := 0
	for b := range buffers {
		count++
		b.release()
	}
	if count != 2 {
		t.Fatalf("admissions=%d", count)
	}
	assertBufferReleased(t, m)
	for _, kind := range []string{"cancel", "read", "storage"} {
		t.Run(kind, func(t *testing.T) {
			m := bufferTestManager(t, kind == "storage", 32, 10)
			if kind == "storage" {
				m.cfg.Storage += "/missing"
			}
			h := bufferTestHandler(t, m, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("failure invoked downstream") }), RequestBuffer("5B").MemoryThreshold("2B"))
			req := httptest.NewRequest("POST", "/", strings.NewReader("abc"))
			want := 503
			if kind == "cancel" {
				ctx, cancel := context.WithCancel(req.Context())
				cancel()
				req = req.WithContext(ctx)
			}
			if kind == "read" {
				req.Body = io.NopCloser(errorReader{})
				want = 400
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != want {
				t.Fatalf("status=%d want=%d", rec.Code, want)
			}
			m.cfg.Storage = ""
			assertBufferReleased(t, m)
		})
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("broken input") }
