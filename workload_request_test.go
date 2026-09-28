package statute

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"statute.kjanat.dev/resolved"
)

// observedWorkloadBody is deliberately non-replayable: the readiness gate
// must neither consume it early nor need GetBody to reconstruct it later.
type observedWorkloadBody struct {
	reader *bytes.Reader
	reads  atomic.Int64
}

func (b *observedWorkloadBody) Read(p []byte) (int, error) {
	b.reads.Add(1)
	return b.reader.Read(p)
}

func (*observedWorkloadBody) Close() error { return nil }

type preservedWorkloadRequest struct {
	method, uri, host string
	headers           []string
	body              []byte
	err               error
}

func TestWorkloadPreservesRequestAcrossReadiness(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		name := "known-length"
		if streaming {
			name = "streamed-non-replayable"
		}
		t.Run(name, func(t *testing.T) { testWorkloadPreservedRequest(t, streaming) })
	}
}

func testWorkloadPreservedRequest(t *testing.T, streaming bool) {
	t.Helper()
	payload := bytes.Repeat([]byte("cold request\x00\xff\n"), 8192)
	received := make(chan preservedWorkloadRequest, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		received <- preservedWorkloadRequest{r.Method, r.RequestURI, r.Host, r.Header.Values("X-Cold-Probe"), body, err}
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(backend.Close)
	policy := testWorkloadPolicy()
	policy.IdleAfter = time.Hour
	policy.Readiness.Mode = resolved.ReadinessDockerHealth
	p, daemon, router := workloadFixture(t, policy, backend.URL)
	daemon.mu.Lock()
	daemon.find("wl-1").health = "starting"
	daemon.mu.Unlock()

	body := &observedWorkloadBody{reader: bytes.NewReader(payload)}
	const uri = "/upload/a%2Fb?key=one&key=two&encoded=%2F%2B&empty="
	req := httptest.NewRequest(http.MethodPost, "http://wl.example.com"+uri, body)
	if !streaming {
		req.ContentLength = int64(len(payload))
	}
	req.Header["X-Cold-Probe"] = []string{"first", "second"}
	if req.GetBody != nil {
		t.Fatal("test request must not be replayable")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan int, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		rw := httptest.NewRecorder()
		router.ServeHTTP(rw, req.WithContext(ctx))
		done <- rw.Code
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Error("request worker did not finish")
		}
	})
	waitDiagnosticWaiters(t, p, 1)
	assertWorkloadRequestBlocked(t, body, received, done)
	daemon.mu.Lock()
	daemon.find("wl-1").health = "healthy"
	daemon.mu.Unlock()
	status := waitStatus(t, done, 5*time.Second, "readiness did not release request")
	if status != http.StatusCreated {
		t.Fatalf("response status = %d, want 201", status)
	}
	assertPreservedWorkloadRequest(t, <-received, payload, uri)
	if got := daemon.startCount("wl-1"); got != 1 {
		t.Fatalf("start calls = %d, want 1", got)
	}
}

func assertWorkloadRequestBlocked(t *testing.T, body *observedWorkloadBody, received <-chan preservedWorkloadRequest, done <-chan int) {
	t.Helper()
	if body.reads.Load() != 0 {
		t.Fatal("request body was consumed before readiness")
	}
	select {
	case <-received:
		t.Fatal("upstream received request before readiness")
	case status := <-done:
		t.Fatalf("request completed before readiness: %d", status)
	default:
	}
}

func assertPreservedWorkloadRequest(t *testing.T, got preservedWorkloadRequest, payload []byte, uri string) {
	t.Helper()
	if got.err != nil || !bytes.Equal(got.body, payload) {
		t.Fatalf("body changed: bytes=%d want=%d error=%v", len(got.body), len(payload), got.err)
	}
	if got.method != http.MethodPost || got.uri != uri || got.host != "wl.example.com" || !reflect.DeepEqual(got.headers, []string{"first", "second"}) {
		t.Fatalf("request metadata changed: method=%s uri=%s host=%s headers=%v", got.method, got.uri, got.host, got.headers)
	}
}
