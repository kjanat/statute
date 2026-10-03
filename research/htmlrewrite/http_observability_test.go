package htmlrewrite

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestHTTPStatuteAbortedStreamObservation(t *testing.T) {
	release := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `<a class="rewrite">private-page-content</a>`)
		w.(http.Flusher).Flush()
		select {
		case <-release:
			panic(http.ErrAbortHandler)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(origin.Close)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	metricsAddr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	endpoint, logs := startHTTPStatuteWithMetrics(t, origin.URL, metricsAddr)
	client := &http.Client{Timeout: 5 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	res, err := client.Get(endpoint + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	prefix := make([]byte, len(`<a class="rewrite" href="https://example.invalid/rewritten">private-page-content<em>inserted</em></a>`))
	if _, err := io.ReadFull(res.Body, prefix); err != nil {
		t.Fatal(err)
	}
	close(release)
	if _, err := io.ReadAll(res.Body); err == nil {
		t.Fatal("failed origin became a complete response")
	}
	entry := awaitProcessAccessLog(t, logs)
	if entry["status"] != float64(200) || entry["aborted"] != true || entry["body_bytes"] != float64(len(prefix)) {
		t.Fatalf("incorrect aborted response: %v", entry)
	}
	if strings.Contains(logs.String(), "private-page-content") {
		t.Fatal("response content leaked into access logs")
	}
	assertProcessAbortMetrics(t, client, metricsAddr, len(prefix))
}

func awaitProcessAccessLog(t *testing.T, logs *processOutput) map[string]any {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		for _, line := range strings.Split(logs.String(), "\n") {
			var entry map[string]any
			if json.Unmarshal([]byte(line), &entry) == nil && entry["path"] == "/stream" {
				return entry
			}
		}
		select {
		case <-timer.C:
			t.Fatalf("aborted request missing from process access log: %s", logs.String())
		case <-tick.C:
		}
	}
}

func assertProcessAbortMetrics(t *testing.T, client *http.Client, addr string, size int) {
	t.Helper()
	wants := []string{
		"statute_requests_total 1\n", "statute_requests_aborted_total 1\n",
		`statute_requests_by_status_total{status="200"} 1`,
		fmt.Sprintf("statute_response_body_bytes_total %d\n", size),
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		res, err := client.Get("http://" + addr + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(wants, func(want string) bool { return !strings.Contains(string(body), want) }) {
			return
		}
		// Access-log output and the outer metrics defer are separate edges.
		select {
		case <-timer.C:
			t.Fatalf("aborted response metrics did not converge:\n%s", body)
		case <-tick.C:
		}
	}
}
