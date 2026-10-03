package htmlrewrite

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"statute.kjanat.dev"
)

// The parent observes a separate Statute process only through HTTP and logs.
func TestHTTPStatuteProcess(t *testing.T) {
	if os.Getenv("STATUTE_HTML_HTTP_CHILD") != "1" {
		return
	}
	e, err := newHTTPEngine(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer e.close()
	base := &http.Transport{DisableCompression: true}
	defer base.CloseIdleConnections()
	makeProxy := func(policy failurePolicy) http.Handler {
		rt := httpTestTransport(t, e, base, testHTTPPolicy(policy))
		return httpTestProxy(t, os.Getenv("STATUTE_HTML_HTTP_ORIGIN"), rt)
	}
	strict, open := makeProxy(failClosed), makeProxy(failOpen)
	statute.Run(statute.Config{
		Listeners: statute.Listeners{statute.HTTP(os.Getenv("STATUTE_HTML_HTTP_ADDR"))},
		Routes: statute.Routes{
			statute.Match("/cached").Handle(strict).With(statute.Cache("1m")),
			statute.Match("/bypass-cached").Handle(open).With(statute.Cache("1m")),
			statute.Match("/retry").Handle(strict).With(statute.Retry(2, statute.OnStatus(503))),
			statute.Match("/compressed").Handle(strict).With(statute.Compress(statute.Gzip)),
			statute.Match("/open").Handle(open),
			statute.Match("/*").Handle(strict),
		},
		Defaults: statute.Defaults{ReadHeaderTimeout: "2s", WriteTimeout: "5s"},
		Shutdown: statute.Shutdown{GracePeriod: "2s", DrainListeners: true},
	})
}

func startHTTPStatute(t *testing.T, origin string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err = ln.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHTTPStatuteProcess$", "-test.v")
	cmd.Env = append(os.Environ(), "STATUTE_HTML_HTTP_CHILD=1", "STATUTE_HTML_HTTP_ORIGIN="+origin, "STATUTE_HTML_HTTP_ADDR="+addr)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err = cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	ready := make(chan struct{})
	logs := make(chan string, 1)
	go func() {
		var lines strings.Builder
		scan := bufio.NewScanner(stderr)
		for scan.Scan() {
			line := scan.Text()
			lines.WriteString(line + "\n")
			if line == "statute: ready" {
				close(ready)
			}
		}
		if err := scan.Err(); err != nil {
			fmt.Fprintln(&lines, err)
		}
		logs <- lines.String()
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		logText := <-logs
		err := cmd.Wait()
		cancel()
		if err != nil {
			t.Errorf("Statute process: %v\n%s\n%s", err, logText, stdout.String())
		}
	})
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("Statute did not become ready")
	}
	return "http://" + addr
}

func TestHTTPStatuteMiddlewareInteractions(t *testing.T) {
	var cacheCalls, retryCalls, bypassCalls atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/cached":
			cacheCalls.Add(1)
		case "/retry":
			if retryCalls.Add(1) == 1 {
				w.WriteHeader(503)
				_, _ = io.WriteString(w, "discarded attempt")
				return
			}
		case "/open", "/strict":
			w.Header().Set("Content-Encoding", "unsupported")
		case "/bypass-cached":
			if bypassCalls.Add(1) == 1 {
				w.Header().Set("Content-Type", "text/html; charset=unsupported")
			}
		}
		_, _ = io.WriteString(w, `<a class="rewrite">page</a>`)
	}))
	defer origin.Close()
	endpoint := startHTTPStatute(t, origin.URL)
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}, Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	get := func(path string, gz bool) (int, http.Header, []byte) {
		t.Helper()
		r := httpTestRequest(t, "GET", endpoint+path)
		if gz {
			r.Header.Set("Accept-Encoding", "gzip")
		}
		res, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var reader io.Reader = res.Body
		if gz {
			if res.Header.Get("Content-Encoding") != "gzip" {
				t.Fatal("compression missing")
			}
			zr, err := gzip.NewReader(res.Body)
			if err != nil {
				t.Fatal(err)
			}
			defer zr.Close()
			reader = zr
		}
		b, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, res.Header, b
	}
	for _, path := range []string{"/cached", "/cached", "/retry", "/compressed"} {
		status, _, b := get(path, path == "/compressed")
		if status != 200 || bytes.Count(b, []byte("<em>inserted</em>")) != 1 {
			t.Fatalf("%s: %d %s", path, status, b)
		}
	}
	if cacheCalls.Load() != 1 || retryCalls.Load() != 2 {
		t.Fatalf("cache/retry counts: %d/%d", cacheCalls.Load(), retryCalls.Load())
	}
	status, _, b := get("/open", false)
	if status != 200 || bytes.Contains(b, []byte("inserted")) {
		t.Fatal("open policy")
	}
	status, _, _ = get("/strict", false)
	if status != 502 {
		t.Fatal("closed policy")
	}
	status, h, b := get("/bypass-cached", false)
	if status != 200 || bytes.Contains(b, []byte("inserted")) || !strings.Contains(h.Get("Cache-Control"), "no-store") {
		t.Fatal("first response must bypass rewriting with no-store")
	}
	if bypassCalls.Load() != 1 {
		t.Fatal("first bypass did not reach origin once")
	}
	for range 2 {
		status, h, b = get("/bypass-cached", false)
		if status != 200 || bytes.Count(b, []byte("<em>inserted</em>")) != 1 || strings.Contains(h.Get("Cache-Control"), "no-store") {
			t.Fatal("recovery must serve one rewritten result")
		}
		if bypassCalls.Load() != 2 {
			t.Fatal("recovery must refetch once and then hit the rewritten cache entry")
		}
	}
}

func TestHTTPStatuteNoTransform(t *testing.T) {
	const input = `<a class="rewrite">page</a>`
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Cache-Control", r.Header.Get("X-Test-Cache-Control"))
		_, _ = io.WriteString(w, input)
	}))
	defer origin.Close()
	endpoint := startHTTPStatute(t, origin.URL)
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	for _, tc := range []struct {
		name, path, requestCache, responseCache string
		status                                  int
		rewritten                               bool
	}{
		{"request closed", "/strict", "no-transform", "public", 502, false},
		{"request open", "/open", "no-transform", "public", 200, false},
		{"response closed", "/strict", "", "no-transform", 502, false},
		{"response open", "/open", "", "no-transform", 200, false},
		{"quoted extension", "/strict", `extension="a, no-transform, b"`, `extension="a, no-transform, b"`, 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httpTestRequest(t, "GET", endpoint+tc.path)
			req.Header.Set("Cache-Control", tc.requestCache)
			req.Header.Set("X-Test-Cache-Control", tc.responseCache)
			res, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			b, err := io.ReadAll(res.Body)
			if err != nil || res.StatusCode != tc.status || bytes.Contains(b, []byte("inserted")) != tc.rewritten {
				t.Fatalf("status=%d body=%q err=%v", res.StatusCode, b, err)
			}
			if tc.status == 200 && !tc.rewritten && (string(b) != input || !strings.Contains(strings.Join(res.Header.Values("Cache-Control"), ","), "no-store")) {
				t.Fatal("bypass must preserve content and prohibit storage")
			}
		})
	}
}
