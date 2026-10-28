package statute

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRequestBufferEarlyRejectionDoesNotWaitForUpload(t *testing.T) {
	for _, kind := range []string{"size", "memory", "slots", "shutdown", "cancelled", "missing-manager"} {
		for _, expectContinue := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/continue=%t", kind, expectContinue), func(t *testing.T) {
				h, status := requestBufferEarlyRejectionHandler(t, kind)
				assertRequestBufferEarlyWireResponse(t, h, kind, status, expectContinue)
			})
		}
	}
}

func requestBufferEarlyRejectionHandler(t *testing.T, kind string) (http.Handler, int) {
	t.Helper()
	m := bufferTestManager(t, false, 6, 0)
	t.Cleanup(func() { awaitBufferRetirement(t, m) })
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("rejected request reached handler") })
	h := bufferTestHandler(t, m, next, RequestBuffer("5B"))
	status := 503
	switch kind {
	case "size":
		status = 413
	case "memory":
		m.cfg.MemoryBudgetBytes = 5
	case "slots":
		m.cfg.MaxInFlight = 1
		held := m.acquire(1)
		t.Cleanup(held.release)
	case "shutdown":
		m.stopAdmission()
	case "cancelled":
		inner := h
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithCancel(r.Context())
			cancel()
			inner.ServeHTTP(w, r.WithContext(ctx))
		})
	case "missing-manager":
		policy, err := resolveMiddleware(RequestBuffer("5B"))
		if err != nil {
			t.Fatal(err)
		}
		h = requestBufferHandler(policy, next)
	}
	return h, status
}

func assertRequestBufferEarlyWireResponse(t *testing.T, h http.Handler, kind string, status int, expectContinue bool) {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	conn, err := net.DialTimeout("tcp", srv.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, requestBufferIncompleteUpload(kind, expectContinue)); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("rejection waited for unfinished upload: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != status {
		t.Fatalf("status=%d want=%d", response.StatusCode, status)
	}
	if status == 503 && response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control=%q", response.Header.Get("Cache-Control"))
	}
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
}

func requestBufferIncompleteUpload(kind string, expectContinue bool) string {
	framing, payload := "Transfer-Encoding: chunked", "1\r\nx\r\n"
	if kind == "size" {
		framing, payload = "Content-Length: 6", "x"
	}
	expect := ""
	if expectContinue {
		expect = "Expect: 100-continue\r\n"
	}
	return fmt.Sprintf("POST / HTTP/1.1\r\nHost: localhost\r\n%s\r\n%s\r\n%s", framing, expect, payload)
}

func TestRequestBufferBeforeCachePreservesEmptyRequests(t *testing.T) {
	for _, proto := range []int{1, 2, 3} {
		for _, tc := range []struct {
			kind  string
			calls int
		}{
			{"nil", 1}, {"no-body", 1}, {"empty-reader", 1}, {"body", 2}, {"trailers", 2},
		} {
			t.Run(fmt.Sprintf("HTTP%d/%s", proto, tc.kind), func(t *testing.T) {
				m := bufferTestManager(t, false, 6, 0)
				calls := 0
				h := bufferTestHandler(t, m, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					if _, err := io.ReadAll(r.Body); err != nil {
						t.Error(err)
					}
					_, _ = io.WriteString(w, "answer")
				}), RequestBuffer("5B"), Cache("1m"))
				for range 2 {
					req := requestBufferCacheRequest(proto, tc.kind)
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, req)
					if rec.Code != 200 || rec.Body.String() != "answer" {
						t.Fatalf("response=%v", rec)
					}
				}
				want := tc.calls
				if proto == 3 {
					want = 2
				}
				if calls != want {
					t.Fatalf("handler calls=%d want=%d", calls, want)
				}
				assertBufferReleased(t, m)
			})
		}
	}
}

func requestBufferCacheRequest(proto int, kind string) *http.Request {
	req := httptest.NewRequest("GET", "/", nil)
	req.ProtoMajor = proto
	switch kind {
	case "nil":
		req.Body = nil
	case "no-body":
		req.Body = http.NoBody
	case "empty-reader":
		req.Body = io.NopCloser(strings.NewReader(""))
		req.ContentLength = -1
	case "body":
		req.Body = io.NopCloser(strings.NewReader("x"))
		req.ContentLength = -1
	case "trailers":
		req.Body = io.NopCloser(strings.NewReader(""))
		req.Trailer = http.Header{"X-Final": {"value"}}
	}
	return req
}
