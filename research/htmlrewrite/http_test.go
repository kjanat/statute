//go:build statute_htmlrewrite

package htmlrewrite

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func httpTestEngine(t *testing.T, concurrency int) *httpEngine {
	t.Helper()
	e, err := newHTTPEngine(context.Background(), concurrency)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.close(); err != nil {
			t.Error(err)
		}
	})
	return e
}

func testHTTPPolicy(f failurePolicy) httpPolicy {
	return httpPolicy{failure: f, inputLimit: 1 << 20, outputLimit: 2 << 20, timeout: 5 * time.Second}
}

func httpTestTransport(t *testing.T, e *httpEngine, base http.RoundTripper, p httpPolicy) *rewriteTransport {
	t.Helper()
	rt, err := newRewriteTransport(e, base, p)
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func httpTestProxy(t *testing.T, target string, rt http.RoundTripper) *httputil.ReverseProxy {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	p := httputil.NewSingleHostReverseProxy(u)
	p.Transport = rt
	p.ErrorLog = log.New(io.Discard, "", 0)
	return p
}

func httpTestRequest(t *testing.T, method, target string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(method, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestHTTPPolicyRequired(t *testing.T) {
	e := httpTestEngine(t, 1)
	for _, f := range []failurePolicy{0, 255} {
		if _, err := newRewriteTransport(e, http.DefaultTransport, testHTTPPolicy(f)); err == nil {
			t.Fatal("implicit/invalid policy accepted")
		}
	}
	p := testHTTPPolicy(failClosed)
	p.timeout = 0
	if _, err := newRewriteTransport(e, http.DefaultTransport, p); err == nil {
		t.Fatal("unbounded request accepted")
	}
}

func TestHTTPPolicyIsolationAndMetadata(t *testing.T) {
	const input = `<a class="rewrite" href="/old">link</a>`
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("ETag", `"original"`)
		w.Header().Set("Last-Modified", "Wed, 21 Oct 2015 07:28:00 GMT")
		w.Header().Set("Digest", "original-digest")
		w.Header().Set("Accept-Ranges", "bytes")
		if r.URL.Path == "/unsupported" {
			w.Header().Set("Content-Encoding", "unknown")
		}
		if r.Header.Get("If-None-Match") != "" || r.Header.Get("Range") != "" {
			w.WriteHeader(304)
			return
		}
		_, _ = io.WriteString(w, input)
	}))
	defer origin.Close()
	e := httpTestEngine(t, 4)
	base := &http.Transport{DisableCompression: true}
	t.Cleanup(base.CloseIdleConnections)
	closed := httpTestTransport(t, e, base, testHTTPPolicy(failClosed))
	open := httpTestTransport(t, e, base, testHTTPPolicy(failOpen))
	for _, rt := range []*rewriteTransport{closed, open} {
		req := httpTestRequest(t, "GET", origin.URL+"/supported")
		req.Header.Set("If-None-Match", `"original"`)
		req.Header.Set("Range", "bytes=0-10")
		res, err := rt.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		b, readErr := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if readErr != nil || !bytes.Contains(b, []byte("<em>inserted</em>")) {
			t.Fatalf("rewrite: %s, %v", b, readErr)
		}
		for _, key := range []string{"ETag", "Last-Modified", "Digest", "Content-Length", "Accept-Ranges"} {
			if res.Header.Get(key) != "" {
				t.Fatalf("stale %s", key)
			}
		}
		if req.Header.Get("Range") == "" || req.Header.Get("If-None-Match") == "" {
			t.Fatal("mutated caller request")
		}
	}
	for range 2 {
		if res, err := closed.RoundTrip(httpTestRequest(t, "GET", origin.URL+"/unsupported")); err == nil {
			_ = res.Body.Close()
			t.Fatal("closed policy bypassed")
		}
		res, err := open.RoundTrip(httpTestRequest(t, "GET", origin.URL+"/unsupported"))
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if err != nil || string(b) != input || res.Header.Get("Content-Encoding") != "unknown" || res.Header.Get("ETag") != `"original"` || !strings.Contains(strings.Join(res.Header.Values("Cache-Control"), ","), "no-store") {
			t.Fatalf("incorrect bypass: %q, %v, %v", b, res.Header, err)
		}
	}
	if closed.rejected.Load() != 2 || open.bypassed.Load() != 2 || open.rejected.Load() != 0 || closed.bypassed.Load() != 0 {
		t.Fatal("route outcomes leaked")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestHTTPEligibility(t *testing.T) {
	e := httpTestEngine(t, 1)
	for _, tc := range []struct {
		name, method, contentType, encoding, cache string
		status                                     int
		trailer                                    bool
		reject                                     bool
	}{
		{"html", "GET", "text/html", "", "", 200, false, false},
		{"head unsupported", "HEAD", "text/html", "unknown", "", 200, false, true},
		{"head", "HEAD", "text/html", "", "", 200, false, false},
		{"post", "POST", "text/html", "unknown", "", 200, false, false},
		{"json", "GET", "application/json", "unknown", "", 200, false, false},
		{"sse", "GET", "text/event-stream", "", "", 200, false, false},
		{"grpc", "GET", "application/grpc", "", "", 200, false, false},
		{"upgrade", "GET", "text/html", "", "", 101, false, false},
		{"bodyless", "GET", "text/html", "", "", 204, false, false},
		{"precondition failed", "GET", "text/html", "", "", 412, false, false},
		{"missing", "GET", "", "", "", 200, false, false},
		{"charset", "GET", "text/html; charset=latin1", "", "", 200, false, true},
		{"encoding", "GET", "text/html", "gzip", "", 200, false, true},
		{"no transform", "GET", "text/html", "", "public, No-Transform", 200, false, true},
		{"invalid type", "GET", "text/html; invalid", "", "", 200, false, true},
		{"trailers", "GET", "text/html", "", "", 200, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, policy := range []failurePolicy{failOpen, failClosed} {
				base := roundTripFunc(func(*http.Request) (*http.Response, error) {
					h := http.Header{"Content-Type": {tc.contentType}, "Content-Encoding": {tc.encoding}, "Cache-Control": {tc.cache}}
					res := &http.Response{StatusCode: tc.status, Header: h, Body: io.NopCloser(strings.NewReader("<p>original</p>")), ContentLength: -1}
					if tc.trailer {
						res.Trailer = http.Header{"Digest": nil}
					}
					return res, nil
				})
				rt := httpTestTransport(t, e, base, testHTTPPolicy(policy))
				res, err := rt.RoundTrip(httpTestRequest(t, tc.method, "http://example.test"))
				if (err != nil) != (tc.reject && policy == failClosed) {
					t.Fatalf("policy %v: %v", policy, err)
				}
				if res != nil {
					_, readErr := io.ReadAll(res.Body)
					_ = res.Body.Close()
					if readErr != nil {
						t.Fatal(readErr)
					}
				}
				wantRewritten := int64(0)
				if tc.name == "html" {
					wantRewritten = 1
				}
				if rt.rewritten.Load() != wantRewritten {
					t.Fatal("response selection changed")
				}
				if tc.reject && policy == failOpen && rt.bypassed.Load() != 1 {
					t.Fatal("unreported bypass")
				}
			}
		})
	}
}

func TestHTTPStreamingAndTerminalFailure(t *testing.T) {
	for _, policy := range []failurePolicy{failOpen, failClosed} {
		t.Run(string(rune('0'+policy)), func(t *testing.T) {
			release := make(chan struct{})
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				_, _ = io.WriteString(w, `<a class="rewrite">first</a>`)
				w.(http.Flusher).Flush()
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				_, _ = io.WriteString(w, strings.Repeat("tail", 4096))
			}))
			defer origin.Close()
			e := httpTestEngine(t, 1)
			p := testHTTPPolicy(policy)
			p.outputLimit = 1024
			rt := httpTestTransport(t, e, http.DefaultTransport, p)
			proxy := httptest.NewServer(httpTestProxy(t, origin.URL, rt))
			defer proxy.Close()
			client := &http.Client{Timeout: 5 * time.Second}
			res, err := client.Get(proxy.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			first := make([]byte, len(`<a class="rewrite" href="https://example.invalid/rewritten">first<em>inserted</em>`))
			n, err := io.ReadFull(res.Body, first)
			if err != nil || !bytes.Contains(first[:n], []byte("inserted")) {
				t.Fatalf("no streaming output before EOF: %q, %v", first[:n], err)
			}
			close(release)
			_, err = io.ReadAll(res.Body)
			if err == nil || rt.failed.Load() != 1 || rt.bypassed.Load() != 0 {
				t.Fatalf("failure became success/fallback: %v", err)
			}
		})
	}
}

func TestHTTPAdmissionCancellationAndShutdown(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer origin.Close()
	e := httpTestEngine(t, 1)
	closed := httpTestTransport(t, e, http.DefaultTransport, testHTTPPolicy(failClosed))
	open := httpTestTransport(t, e, http.DefaultTransport, testHTTPPolicy(failOpen))
	res, err := closed.RoundTrip(httpTestRequest(t, "GET", origin.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if other, err := closed.RoundTrip(httpTestRequest(t, "GET", origin.URL)); err == nil {
		_ = other.Body.Close()
		t.Fatal("capacity ignored")
	}
	other, err := open.RoundTrip(httpTestRequest(t, "GET", origin.URL))
	if err != nil {
		t.Fatal(err)
	}
	_ = other.Body.Close()
	done := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, res.Body); done <- err }()
	if err := e.close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("shutdown reported clean EOF")
		}
	case <-time.After(time.Second):
		t.Fatal("reader survived shutdown")
	}
	e.mu.Lock()
	n := len(e.active)
	e.mu.Unlock()
	if n != 0 {
		t.Fatal("leaked admission")
	}
	if other, err := closed.RoundTrip(httpTestRequest(t, "GET", origin.URL)); err == nil {
		_ = other.Body.Close()
		t.Fatal("admitted after shutdown")
	}
	other, err = open.RoundTrip(httpTestRequest(t, "GET", origin.URL))
	if err != nil {
		t.Fatal(err)
	}
	_ = other.Body.Close()
	if open.bypassed.Load() != 2 {
		t.Fatal("shutdown ignored open policy")
	}
}

func TestHTTPDeadlineAndKnownLength(t *testing.T) {
	e := httpTestEngine(t, 1)
	var closes atomic.Int64
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html"}}, ContentLength: -1, Body: &contextBody{ctx: r.Context(), closes: &closes}}, nil
	})
	p := testHTTPPolicy(failClosed)
	p.timeout = 200 * time.Millisecond
	rt := httpTestTransport(t, e, base, p)
	res, err := rt.RoundTrip(httpTestRequest(t, "GET", "http://example.test"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(res.Body)
	_ = res.Body.Close()
	if !errors.Is(err, context.DeadlineExceeded) || closes.Load() != 1 {
		t.Fatalf("deadline/close: %v, %d", err, closes.Load())
	}
	for _, policy := range []failurePolicy{failOpen, failClosed} {
		base := roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html"}}, ContentLength: 100, Body: io.NopCloser(strings.NewReader("unread"))}, nil
		})
		p := testHTTPPolicy(policy)
		p.inputLimit = 10
		rt := httpTestTransport(t, e, base, p)
		res, err := rt.RoundTrip(httpTestRequest(t, "GET", "http://example.test"))
		if (err != nil) != (policy == failClosed) {
			t.Fatal("known-length policy")
		}
		if res != nil {
			_ = res.Body.Close()
		}
	}
}

type contextBody struct {
	ctx    context.Context
	closes *atomic.Int64
}

func (b *contextBody) Read([]byte) (int, error) { <-b.ctx.Done(); return 0, b.ctx.Err() }
func (b *contextBody) Close() error             { b.closes.Add(1); return nil }
