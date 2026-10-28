package statute

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRequestBufferSmallBodiesReserveActualCapacity(t *testing.T) {
	for _, disk := range []bool{false, true} {
		for _, kind := range []string{"nil", "no-body", "known", "unknown", "zero-length"} {
			t.Run(fmtBufferCase(disk, 0, false)+"/"+kind, func(t *testing.T) {
				m := bufferTestManager(t, disk, 2048, 1<<20)
				req, want, size := requestBufferSmallRequest(kind)
				var retained io.ReadCloser
				h := bufferTestHandler(t, m, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					var err error
					retained, err = r.GetBody()
					if err != nil {
						t.Error(err)
					}
				}), RequestBuffer("1MiB"))
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if retained == nil {
					t.Fatalf("small request rejected: status=%d", rec.Code)
				}
				defer retained.Close()
				m.mu.Lock()
				memory, active := m.memory, m.active
				m.mu.Unlock()
				if memory != size || active != 1 {
					t.Errorf("retained memory=%d active=%d, want %d and 1", memory, active, size)
				}
				data, err := io.ReadAll(retained)
				if err != nil || string(data) != want {
					t.Errorf("replay=%q error=%v", data, err)
				}
				_ = retained.Close()
				assertBufferReleased(t, m)
			})
		}
	}
}

func requestBufferSmallRequest(kind string) (*http.Request, string, int64) {
	req := httptest.NewRequest("POST", "/", strings.NewReader("abc"))
	want, size := "abc", int64(4)
	switch kind {
	case "nil":
		req.Body, want, size = nil, "", 0
	case "no-body":
		req.Body, want, size = http.NoBody, "", 0
	case "unknown":
		req.ContentLength, size = -1, 512
	case "zero-length":
		req.ContentLength, size = 0, 512
	}
	return req, want, size
}

func TestRequestBufferGrowthAndSpillReplay(t *testing.T) {
	for _, disk := range []bool{false, true} {
		for _, length := range []int64{-1, 1} {
			t.Run(fmtBufferCase(disk, int(length), false), func(t *testing.T) {
				m := bufferTestManager(t, disk, 4096, 2048)
				payload := strings.Repeat("x", 1537)
				req := httptest.NewRequest("PUT", "/", strings.NewReader(payload))
				req.ContentLength = length // A length hint must never truncate the reader.
				calls := 0
				h := bufferTestHandler(t, m, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					assertRequestBufferReplay(t, r, payload)
					if calls == 1 {
						w.WriteHeader(503)
					}
				}), RequestBuffer("2KiB").MemoryThreshold("512B"), Retry(2, OnStatus(503)))
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != 200 || calls != 2 {
					t.Fatalf("status=%d calls=%d", rec.Code, calls)
				}
				assertBufferReleased(t, m)
			})
		}
	}
}

func TestRequestBufferGrowthFailureDoesNotDispatch(t *testing.T) {
	for _, disk := range []bool{false, true} {
		t.Run(fmtBufferCase(disk, 0, true), func(t *testing.T) {
			m := bufferTestManager(t, disk, 1024, 2048)
			calls := 0
			h := bufferTestHandler(t, m, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }), RequestBuffer("2KiB").MemoryThreshold("512B"))
			req := httptest.NewRequest("POST", "/", strings.NewReader(strings.Repeat("x", 513)))
			req.ContentLength = -1
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != 503 || calls != 0 || rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status=%d calls=%d headers=%v", rec.Code, calls, rec.Header())
			}
			assertBufferReleased(t, m)
		})
	}
}

func TestRequestBufferGrowthChargesOverlappingAllocations(t *testing.T) {
	m := bufferTestManager(t, false, 8, 0)
	b := m.acquire(3)
	copy(b.data, "abc")
	b.size = 3
	if b.grow(6) {
		t.Fatal("growth admitted 3+6 bytes into an 8-byte budget")
	}
	if b.memory != 3 || !bytes.Equal(b.data, []byte("abc")) {
		t.Fatal("failed growth changed the buffer")
	}
	m.cfg.MemoryBudgetBytes = 9
	if !b.grow(6) || b.memory != 6 || string(b.data[:3]) != "abc" {
		t.Fatal("growth failed at exact overlap capacity")
	}
	m.mu.Lock()
	memory := m.memory
	m.mu.Unlock()
	if memory != 6 {
		t.Fatalf("old allocation still charged: %d", memory)
	}
	b.release()
	assertBufferReleased(t, m)
}

func TestRequestBufferSmallConcurrentReadersShareBudget(t *testing.T) {
	m := bufferTestManager(t, false, 64, 0)
	readers := make(chan io.ReadCloser, 32)
	h := bufferTestHandler(t, m, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reader, err := r.GetBody()
		if err != nil {
			t.Error(err)
			return
		}
		readers <- reader
	}), RequestBuffer("5MiB"))
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/", strings.NewReader("x")))
			if rec.Code != 200 {
				t.Errorf("small upload status=%d", rec.Code)
			}
		})
	}
	wg.Wait()
	close(readers)
	m.mu.Lock()
	memory, active := m.memory, m.active
	m.mu.Unlock()
	if memory != 64 || active != 32 {
		t.Errorf("memory=%d active=%d, want 64 and 32", memory, active)
	}
	m.stopAdmission()
	m.cancel()
	for reader := range readers {
		data, err := io.ReadAll(reader)
		if err != nil || string(data) != "x" {
			t.Errorf("late replay=%q error=%v", data, err)
		}
		_ = reader.Close()
	}
	assertBufferReleased(t, m)
}

func TestRequestBufferGrownAllocationSurvivesTimeout(t *testing.T) {
	m := bufferTestManager(t, false, 1536, 0)
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	payload := strings.Repeat("x", 600)
	h := bufferTestHandler(t, m, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		defer close(finished)
		close(entered)
		<-release
		assertRequestBufferReplay(t, r, payload)
	}), RequestBuffer("1MiB"), Timeout("20ms"))
	req := httptest.NewRequest("POST", "/", strings.NewReader(payload))
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	<-entered
	if rec.Code != 503 {
		t.Errorf("timeout status=%d", rec.Code)
	}
	m.mu.Lock()
	memory := m.memory
	m.mu.Unlock()
	if memory != 1024 {
		t.Errorf("late producer charge=%d, want 1024", memory)
	}
	calls := 0
	sibling := bufferTestHandler(t, m, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }), RequestBuffer("1MiB"))
	rec = httptest.NewRecorder()
	sibling.ServeHTTP(rec, httptest.NewRequest("POST", "/", strings.NewReader(strings.Repeat("y", 512))))
	if rec.Code != 503 || calls != 0 {
		t.Errorf("sibling status=%d calls=%d", rec.Code, calls)
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("late producer did not finish")
	}
	awaitBufferRetirement(t, m)
}

func TestRequestBufferOversizeAfterGrowth(t *testing.T) {
	for _, disk := range []bool{false, true} {
		t.Run(fmtBufferCase(disk, 2049, true), func(t *testing.T) {
			m := bufferTestManager(t, disk, 8192, 2048)
			calls := 0
			h := bufferTestHandler(t, m, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }), RequestBuffer("2KiB").MemoryThreshold("512B"))
			req := httptest.NewRequest("POST", "/", strings.NewReader(strings.Repeat("x", 2049)))
			req.ContentLength = -1
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != 413 || calls != 0 {
				t.Fatalf("status=%d calls=%d", rec.Code, calls)
			}
			assertBufferReleased(t, m)
		})
	}
}
