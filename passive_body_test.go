package statute

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// A 5xx response and its truncated body belong to the same backend attempt.
func TestPassiveTruncated5xxCountsOnce(t *testing.T) {
	t.Parallel()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "short")
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(origin.Close)
	ph := newPassivePoolHandler(t, nil, time.Minute, 2, origin.URL)
	running := ph.start()
	t.Cleanup(running.shutdown)
	front := httptest.NewServer(ph)
	t.Cleanup(front.Close)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(front.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr == nil {
		t.Fatal("truncated body unexpectedly succeeded")
	}
	if got := passiveFailureCount(ph.passive.Load(), ph.primary[0]); got != 1 {
		t.Fatalf("one 503 with truncated body counted %d failures", got)
	}
}

func passiveFailureCount(run *passiveRun, backend *backendState) int {
	run.mu.Lock()
	defer run.mu.Unlock()
	return len(run.failures[backend])
}

type passiveHeaderSignal struct {
	*httptest.ResponseRecorder
	ready chan struct{}
	once  sync.Once
}

func (w *passiveHeaderSignal) WriteHeader(status int) {
	w.ResponseRecorder.WriteHeader(status)
	w.once.Do(func() { close(w.ready) })
}

func waitPassiveSignal(t *testing.T, ready <-chan struct{}) {
	t.Helper()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("proxy did not reach the expected response boundary")
	}
}

func TestPassiveBodyCancelAfterHeaders(t *testing.T) {
	t.Parallel()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(origin.Close)
	ph := newPassivePoolHandler(t, nil, time.Minute, 1, origin.URL)
	running := ph.start()
	t.Cleanup(running.shutdown)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &passiveHeaderSignal{ResponseRecorder: httptest.NewRecorder(), ready: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ph.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx))
	}()
	waitPassiveSignal(t, w.ready)
	cancel()
	waitPassiveSignal(t, done)
	if got := passiveFailureCount(ph.passive.Load(), ph.primary[0]); got != 0 {
		t.Fatalf("client cancellation after headers counted %d failures", got)
	}
}

func TestPassiveLateBodyErrorCannotReachSuccessor(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(w, "first")
		w.(http.Flusher).Flush()
		<-release
	}))
	t.Cleanup(origin.Close)
	ph := newPassivePoolHandler(t, nil, time.Minute, 1, origin.URL)
	first := ph.start()
	t.Cleanup(first.shutdown)
	w := &passiveHeaderSignal{ResponseRecorder: httptest.NewRecorder(), ready: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ph.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	}()
	waitPassiveSignal(t, w.ready)
	first.shutdown()
	second := ph.start()
	t.Cleanup(second.shutdown)
	releaseOnce()
	waitPassiveSignal(t, done)
	if got := passiveFailureCount(second.passive, ph.primary[0]); got != 0 {
		t.Fatalf("retired body's error counted %d failures in successor", got)
	}
}

type passiveErrorBody struct {
	err, closeErr error
}

func (b *passiveErrorBody) Read([]byte) (int, error) { return 0, b.err }
func (b *passiveErrorBody) Close() error             { return b.closeErr }

func TestPassiveBodyPreservesErrors(t *testing.T) {
	t.Parallel()
	for _, readErr := range []error{io.EOF, io.ErrUnexpectedEOF, context.Canceled} {
		t.Run(readErr.Error(), func(t *testing.T) {
			closeErr := errors.New("close failed")
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r = r.WithContext(context.WithValue(r.Context(), passiveAttemptKey{}, &passiveAttempt{}))
			failures := 0
			body := &passiveResponseBody{
				ReadCloser: &passiveErrorBody{err: readErr, closeErr: closeErr}, request: r,
				record: func(*http.Request) { failures++ },
			}
			for range 2 {
				if n, err := body.Read(make([]byte, 1)); n != 0 || err != readErr { //nolint:errorlint // The observer must preserve error identity, including no extra wrapping.
					t.Fatalf("read changed: n=%d err=%v", n, err)
				}
			}
			if err := body.Close(); err != closeErr { //nolint:errorlint // Close must return the identical underlying error.
				t.Fatalf("close changed: %v", err)
			}
			want := 0
			if errors.Is(readErr, io.ErrUnexpectedEOF) {
				want = 1
			}
			if failures != want {
				t.Fatalf("failures=%d; want %d", failures, want)
			}
		})
	}
}

func TestPassiveTruncatedBodyDemotesBackend(t *testing.T) {
	t.Parallel()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "short")
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(bad.Close)
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "healthy")
	}))
	t.Cleanup(good.Close)
	ph := newPassivePoolHandler(t, nil, time.Minute, 1, bad.URL, good.URL)
	running := ph.start()
	t.Cleanup(running.shutdown)
	front := httptest.NewServer(ph)
	t.Cleanup(front.Close)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(front.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || readErr == nil {
		t.Fatalf("truncated response: status=%d read error=%v", resp.StatusCode, readErr)
	}
	if !ph.passive.Load().demoted(ph.primary[0]) {
		t.Fatal("truncated 200 response did not demote its backend")
	}
	for range 2 {
		resp, err = client.Get(front.URL)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || strings.TrimSpace(string(body)) != "healthy" {
			t.Fatalf("after demotion: body=%q read error=%v", body, err)
		}
	}
}
