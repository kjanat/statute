//go:build e2e

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// startOrigin serves a fresh origin on a real loopback socket — the lane
// forbids httptest by contract, and these tests honor it.
func startOrigin(t *testing.T) string {
	t.Helper()
	o := newOrigin("origin-test")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: o, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return "http://" + ln.Addr().String()
}

func getJSON(t *testing.T, url string, out any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Request-Id", "rid-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			t.Fatalf("parse %q: %v", body, err)
		}
	}
}

func TestOriginEchoIdentity(t *testing.T) {
	t.Parallel()
	base := startOrigin(t)
	var echo echoBody
	getJSON(t, base+"/echo?x=1", &echo)
	if echo.Origin != "origin-test" || echo.Path != "/echo" || echo.Query != "x=1" || echo.RequestID != "rid-1" {
		t.Errorf("echo: %+v", echo)
	}
}

func TestOriginFailBudget(t *testing.T) {
	t.Parallel()
	base := startOrigin(t)
	for i, want := range []int{502, 502, 200, 200} {
		resp, err := http.Get(base + "/fail?key=k1&n=2")
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("request %d: got %d, want %d", i, resp.StatusCode, want)
		}
	}
}

func TestOriginHealthToggle(t *testing.T) {
	t.Parallel()
	base := startOrigin(t)
	status := func() int {
		resp, err := http.Get(base + "/health")
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := status(); got != 200 {
		t.Fatalf("initial health: %d", got)
	}
	http.Get(base + "/admin/health?state=down")
	if got := status(); got != 503 {
		t.Errorf("after down: %d", got)
	}
	http.Get(base + "/admin/health?state=up")
	if got := status(); got != 200 {
		t.Errorf("after up: %d", got)
	}
}

// TestOriginJournalRecordsAsSeenPath — rewrite assertions depend on the
// journal holding the request exactly as received.
func TestOriginJournalRecordsAsSeenPath(t *testing.T) {
	t.Parallel()
	base := startOrigin(t)
	getJSON(t, base+"/some/rewritten/path?q=1", nil)
	var entries []journalEntry
	getJSON(t, base+"/admin/requests", &entries)
	if len(entries) != 1 {
		t.Fatalf("journal: %d entries (admin traffic must not be recorded)", len(entries))
	}
	if entries[0].Path != "/some/rewritten/path" || entries[0].RequestID != "rid-1" {
		t.Errorf("journal entry: %+v", entries[0])
	}
}

func TestOriginUpgradeEcho(t *testing.T) {
	t.Parallel()
	base := startOrigin(t)
	addr := strings.TrimPrefix(base, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET /upgrade HTTP/1.1\r\nHost: %s\r\nUpgrade: echo\r\nConnection: Upgrade\r\n\r\n", addr)
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "101") {
		t.Fatalf("status: %q", status)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	io.WriteString(conn, "hello\n")
	echo, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if echo != "origin-test: hello\n" {
		t.Errorf("echo: %q", echo)
	}
}

func TestOriginStreamDeliversChunksEarly(t *testing.T) {
	t.Parallel()
	base := startOrigin(t)
	start := time.Now()
	resp, err := http.Get(base + "/stream?chunks=3&interval=200ms")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	firstByte := time.Since(start)
	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatal(err)
	}
	total := time.Since(start)
	if lines := strings.Count(string(rest), "\n"); lines != 2 {
		t.Errorf("remaining lines: %d", lines)
	}
	if firstByte*2 > total {
		t.Errorf("first line at %s of %s total; chunks were not flushed early", firstByte, total)
	}
}
