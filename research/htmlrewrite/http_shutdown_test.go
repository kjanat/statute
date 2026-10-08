//go:build statute_htmlrewrite

package htmlrewrite

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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
	err := pollHTTPListenerClosed(func() error {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
		}
		return err
	}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
}

func pollHTTPListenerClosed(probe func() error, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		err := probe()
		if errors.Is(err, syscall.ECONNREFUSED) {
			return nil
		}
		// Reset and timeout are inconclusive while shutdown closes ingress.
		// Keep probing within the deadline; only a refused dial proves closure.
		var netErr net.Error
		timedOut := errors.As(err, &netErr) && netErr.Timeout()
		if err != nil && !errors.Is(err, syscall.ECONNRESET) && !timedOut {
			return fmt.Errorf("checking closed ingress: %w", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("shutdown did not close ingress: %w", context.DeadlineExceeded)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPollHTTPListenerClosed(t *testing.T) {
	reset := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNRESET}
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	timedOut := &net.OpError{Op: "dial", Net: "tcp", Err: context.DeadlineExceeded}
	for _, tc := range []struct {
		name     string
		outcomes []error
		timeout  time.Duration
		want     error
	}{
		{"refused", []error{refused}, time.Second, nil},
		{"reset then open then refused", []error{reset, nil, refused}, time.Second, nil},
		{"persistent reset", []error{reset}, 0, context.DeadlineExceeded},
		{"still open", []error{nil}, 0, context.DeadlineExceeded},
		{"unexpected error", []error{syscall.EACCES}, time.Second, syscall.EACCES},
		{"timeout then reset then open then refused", []error{timedOut, reset, nil, refused}, time.Second, nil},
		{"system timeout then refused", []error{syscall.ETIMEDOUT, refused}, time.Second, nil},
		{"persistent dial timeout", []error{timedOut}, 0, context.DeadlineExceeded},
		{"persistent system timeout", []error{syscall.ETIMEDOUT}, 0, context.DeadlineExceeded},
		{"timeout then unexpected error", []error{timedOut, syscall.EACCES}, time.Second, syscall.EACCES},
		{"reset then unexpected error", []error{reset, syscall.EACCES}, time.Second, syscall.EACCES},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			err := pollHTTPListenerClosed(func() error {
				if calls >= len(tc.outcomes) {
					t.Fatal("unexpected extra probe")
				}
				outcome := tc.outcomes[calls]
				calls++
				return outcome
			}, tc.timeout)
			if !errors.Is(err, tc.want) || calls != len(tc.outcomes) {
				t.Fatalf("error=%v want=%v probes=%d want=%d", err, tc.want, calls, len(tc.outcomes))
			}
		})
	}
}

func TestHTTPStatuteFailedControlCleanup(t *testing.T) {
	const marker = "STATUTE_HTML_HTTP_CLEANUP_FAILURE=1"
	const failure = "intentional failure before control shutdown"
	if os.Getenv("STATUTE_HTML_HTTP_CLEANUP_FAILURE") == "1" {
		startHTTPStatuteConfigured(t, "http://127.0.0.1:1", "", []string{"STATUTE_HTML_HTTP_SHUTDOWN_CONTROL=1"})
		t.Fatal(failure)
	}
	// Keep the outer process alive beyond its child's 30-second safety deadline
	// so a regression can still reap that child before this test reports failure.
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHTTPStatuteFailedControlCleanup$")
	cmd.Env = append(os.Environ(), marker)
	started := time.Now()
	out, err := cmd.CombinedOutput()
	if err == nil || ctx.Err() != nil || time.Since(started) >= 10*time.Second || !strings.Contains(string(out), failure) {
		t.Fatalf("failed-path cleanup: error=%v context=%v\n%s", err, ctx.Err(), out)
	}
}
