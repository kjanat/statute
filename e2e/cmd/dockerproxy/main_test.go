//go:build e2e

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// serveRecorderTest uses real TCP sockets, matching the lane's no-httptest rule.
func serveRecorderTest(t *testing.T, handler http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); <-done })
	return ln.Addr().String()
}

func testRecorderTransport(t *testing.T, addr string) *http.Transport {
	t.Helper()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	return transport
}

func TestRecorderCountsEveryStartBeforeForwarding(t *testing.T) {
	t.Parallel()
	const id = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const path = "/v1.47/containers/" + id + "/start?detachKeys=ctrl%2Fp"
	var recorder *recorder
	upstream := serveRecorderTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertForwardedStart(t, r, path)
		count := recorder.snapshot().Starts[id]
		if count == 0 {
			t.Error("request reached upstream before being recorded")
		}
		if count == 1 {
			w.WriteHeader(http.StatusNoContent)
		} else {
			w.WriteHeader(http.StatusNotModified)
		}
	}))
	recorder = newRecorder(testRecorderTransport(t, upstream))
	proxy := serveRecorderTest(t, recorder)
	client := &http.Client{Timeout: 3 * time.Second}
	for _, want := range []int{http.StatusNoContent, http.StatusNotModified} {
		postRecordedStart(t, client, "http://"+proxy+path, want)
	}
	got := readStartSnapshot(t, client, "http://"+proxy+"/debug/starts")
	if got.Total != 2 || got.Starts[id] != 2 || len(got.Starts) != 1 {
		t.Fatalf("start counters: %+v", got)
	}
	got.Starts[id] = 99
	if recorder.snapshot().Starts[id] != 2 {
		t.Fatal("snapshot aliases live counters")
	}
}

func readStartSnapshot(t *testing.T, client *http.Client, url string) startSnapshot {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got startSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

func assertForwardedStart(t *testing.T, r *http.Request, path string) {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil || string(body) != "payload" || r.Method != http.MethodPost || r.RequestURI != path || r.Header.Get("X-Probe") != "preserved" {
		t.Errorf("forwarded request: %s %s, header=%q body=%q error=%v", r.Method, r.RequestURI, r.Header.Get("X-Probe"), body, err)
	}
}

func postRecordedStart(t *testing.T, client *http.Client, url string, want int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Probe", "preserved")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("status=%d, want %d", resp.StatusCode, want)
	}
}

func TestRecorderStreamsEventsBeforeEOF(t *testing.T) {
	t.Parallel()
	upstream := serveRecorderTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1.47/events" || r.URL.RawQuery != "filters=container" {
			t.Errorf("event URL: %s", r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{\"Action\":\"start\"}\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	recorder := newRecorder(testRecorderTransport(t, upstream))
	proxy := serveRecorderTest(t, recorder)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+proxy+"/v1.47/events?filters=container", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "{\"Action\":\"start\"}\n" {
		t.Fatalf("streamed event=%q error=%v", line, err)
	}
	if got := recorder.snapshot().Total; got != 0 {
		t.Fatalf("event stream counted as start: %d", got)
	}
}

func TestStartReferencePaths(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ method, path, want string }{
		{http.MethodPost, "/containers/abc/start", "abc"},
		{http.MethodPost, "/v1.47/containers/abc/start?x=y", "abc"},
		{http.MethodGet, "/containers/abc/start", ""},
		{http.MethodPost, "/containers/abc/stop", ""},
		{http.MethodPost, "/containers/abc/start/extra", ""},
		{http.MethodPost, "/containers//start", ""},
	} {
		req, err := http.NewRequest(tc.method, "http://docker"+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := startReference(req); got != tc.want {
			t.Errorf("%s %s: %q, want %q", tc.method, tc.path, got, tc.want)
		}
	}
}
