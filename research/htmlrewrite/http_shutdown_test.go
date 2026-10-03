package htmlrewrite

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestHTTPStatuteDrainsRewrittenStream(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	finishOrigin := func() { once.Do(func() { close(release) }) }
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `<a class="rewrite">prefix</a>`)
		w.(http.Flusher).Flush()
		select {
		case <-release:
			_, _ = io.WriteString(w, `<a class="rewrite">tail</a>`)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(origin.Close)
	t.Cleanup(finishOrigin)
	endpoint, _ := startHTTPStatuteConfigured(t, origin.URL, "", []string{"STATUTE_HTML_HTTP_SHUTDOWN_CONTROL=1"})
	client := &http.Client{Timeout: 5 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	res, err := client.Get(endpoint + "/strict")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	prefix := make([]byte, len(`<a class="rewrite" href="https://example.invalid/rewritten">prefix<em>inserted</em>`))
	if _, err := io.ReadFull(res.Body, prefix); err != nil || !strings.Contains(string(prefix), "<em>inserted</em>") {
		t.Fatalf("rewritten prefix: %q %v", prefix, err)
	}
	shutdown, err := client.Post(endpoint+"/_research/shutdown", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = shutdown.Body.Close()
	if shutdown.StatusCode != http.StatusNoContent {
		t.Fatalf("shutdown control: %d", shutdown.StatusCode)
	}
	waitHTTPListenerClosed(t, strings.TrimPrefix(endpoint, "http://"))
	finishOrigin()
	tail, err := io.ReadAll(res.Body)
	if err != nil || !strings.Contains(string(tail), "tail<em>inserted</em>") || strings.Count(string(tail), "<em>inserted</em>") != 1 {
		t.Fatalf("draining lost the rewritten tail: %q %v", tail, err)
	}
	// The child-process cleanup requires a clean exit after its drain completes.
}

func waitHTTPListenerClosed(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			if !errors.Is(err, syscall.ECONNREFUSED) {
				t.Fatalf("checking closed ingress: %v", err)
			}
			return
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("shutdown did not close ingress")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
