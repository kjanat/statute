//go:build htmlrewrite_research

package htmlrewrite

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Hold the final response EOF after the origin and rewriter have finished.
type rewriteEndBarrier struct {
	arrived atomic.Bool
	once    sync.Once
	release chan struct{}
}

func newRewriteEndBarrier() *rewriteEndBarrier {
	return &rewriteEndBarrier{release: make(chan struct{})}
}

func (e *rewriteEndBarrier) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		e.once.Do(func() { close(e.release) })
		w.WriteHeader(204)
		return
	}
	if e.arrived.Load() {
		w.WriteHeader(204)
	} else {
		w.WriteHeader(202)
	}
}

func (e *rewriteEndBarrier) wrap(rt http.RoundTripper) http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		res, err := rt.RoundTrip(r)
		if err == nil && r.Header.Get("X-Test-Hold-Rewrite-End") == "yes" {
			res.Body = &rewriteEndBody{ReadCloser: res.Body, barrier: e, ctx: r.Context()}
		}
		return res, err
	})
}

type rewriteEndBody struct {
	io.ReadCloser
	barrier *rewriteEndBarrier
	ctx     context.Context
}

func (b *rewriteEndBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) {
		b.barrier.arrived.Store(true)
		select {
		case <-b.barrier.release:
		case <-b.ctx.Done():
			return n, b.ctx.Err()
		}
	}
	return n, err
}

func TestHTTPDockerLeaseCoversRewrittenResponseEnd(t *testing.T) {
	for _, completion := range []string{"complete", "cancel"} {
		t.Run(completion, func(t *testing.T) { testDockerRewrittenResponseEnd(t, completion == "cancel") })
	}
}

func testDockerRewrittenResponseEnd(t *testing.T, cancelResponse bool) {
	t.Helper()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `<a class="rewrite">complete</a>`)
	}))
	t.Cleanup(origin.Close)
	endpoint, d := startDockerRewriteStatute(t, origin.URL)
	client := &http.Client{Timeout: 5 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	req := httpTestRequest(t, "GET", endpoint)
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	req = req.WithContext(ctx)
	req.Host = "rewrite.test"
	req.Header.Set("X-Test-Hold-Rewrite-End", "yes")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	prefix := make([]byte, len(`<a class="rewrite" href="https://example.invalid/rewritten">complete<em>inserted</em>`))
	if _, err := io.ReadFull(res.Body, prefix); err != nil || !strings.Contains(string(prefix), "inserted") {
		t.Fatalf("prefix: %q %v", prefix, err)
	}
	waitDockerRouteStatus(t, client, endpoint+"/_research/end", "rewrite.test", 204)
	baseline := d.stopCount("old-container")
	time.Sleep(600 * time.Millisecond)
	if d.stopCount("old-container") != baseline {
		t.Fatal("origin EOF released the workload before response completion")
	}
	if cancelResponse {
		cancel()
		_, err := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled response read: %v", err)
		}
		waitDockerStop(t, d, "old-container", baseline)
		return
	}
	release, err := client.Post(endpoint+"/_research/end", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = release.Body.Close()
	if release.StatusCode != 204 {
		t.Fatal("response-end barrier was not released")
	}
	if _, err := io.ReadAll(res.Body); err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	waitDockerStop(t, d, "old-container", baseline)
}
