package statute

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestRequestBufferWaitsForEOF(t *testing.T) {
	for _, oversize := range []bool{false, true} {
		t.Run(fmtBufferCase(true, 5, oversize), func(t *testing.T) {
			m := bufferTestManager(t, true, 8, 5)
			var calls atomic.Int32
			h := bufferTestHandler(t, m, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				data, err := io.ReadAll(r.Body)
				if err != nil || string(data) != "abcde" {
					t.Errorf("body=%q error=%v", data, err)
				}
			}), RequestBuffer("5B").MemoryThreshold("2B"))
			reader, writer := io.Pipe()
			req := httptest.NewRequest("POST", "/", reader)
			done := make(chan struct{})
			rec := httptest.NewRecorder()
			go func() { defer close(done); h.ServeHTTP(rec, req) }()
			if _, err := writer.Write([]byte("abc")); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 0 {
				t.Fatal("downstream started before EOF")
			}
			suffix := "de"
			if oversize {
				suffix = "def"
			}
			_, _ = writer.Write([]byte(suffix))
			_ = writer.Close()
			<-done
			want, count := 200, int32(1)
			if oversize {
				want, count = 413, 0
			}
			if rec.Code != want || calls.Load() != count {
				t.Fatalf("status=%d calls=%d", rec.Code, calls.Load())
			}
			assertBufferReleased(t, m)
		})
	}
}

func TestRequestBufferSlotLimit(t *testing.T) {
	m := bufferTestManager(t, false, 1024, 0)
	buffers := make([]*requestBodyBuffer, 0, requestBufferMaxInFlight)
	for range requestBufferMaxInFlight {
		b := m.acquire(2)
		if b == nil {
			t.Fatal("early exhaustion")
		}
		buffers = append(buffers, b)
	}
	if b := m.acquire(2); b != nil {
		b.release()
		t.Fatal("slot limit exceeded")
	}
	for _, b := range buffers {
		b.release()
	}
	assertBufferReleased(t, m)
}
