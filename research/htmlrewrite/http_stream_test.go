package htmlrewrite

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPDisconnectReleasesInstance(t *testing.T) {
	originDone := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(originDone)
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<p>first</p>")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer origin.Close()
	e := httpTestEngine(t, 1)
	rt := httpTestTransport(t, e, http.DefaultTransport, testHTTPPolicy(failClosed))
	proxy := httpTestProxy(t, origin.URL, rt)
	proxyDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(proxyDone)
		proxy.ServeHTTP(w, r)
	}))
	defer server.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	res, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	first := make([]byte, len("<p>first</p>"))
	if _, err := io.ReadFull(res.Body, first); err != nil {
		_ = res.Body.Close()
		t.Fatal(err)
	}
	_ = res.Body.Close()
	for _, done := range []chan struct{}{originDone, proxyDone} {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("disconnect left a handler active")
		}
	}
	e.mu.Lock()
	remaining := len(e.active)
	e.mu.Unlock()
	if remaining != 0 {
		t.Fatal("disconnect leaked rewrite capacity")
	}
}

func TestHTTPPullAndUnknownLengthLimit(t *testing.T) {
	e := httpTestEngine(t, 1)
	var reads atomic.Int64
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html"}}, ContentLength: -1, Body: &countingBody{Reader: strings.NewReader(strings.Repeat("x", 100)), reads: &reads}}, nil
	})
	for _, policy := range []failurePolicy{failOpen, failClosed} {
		reads.Store(0)
		p := testHTTPPolicy(policy)
		p.inputLimit = 10
		rt := httpTestTransport(t, e, base, p)
		res, err := rt.RoundTrip(httpTestRequest(t, "GET", "http://example.test"))
		if err != nil {
			t.Fatal(err)
		}
		if reads.Load() != 0 {
			t.Fatal("read ahead without downstream demand")
		}
		_, err = io.ReadAll(res.Body)
		_ = res.Body.Close()
		if err == nil || rt.bypassed.Load() != 0 || rt.failed.Load() != 1 {
			t.Fatal("unknown-length limit bypassed")
		}
	}
}

type countingBody struct {
	io.Reader
	reads *atomic.Int64
}

func (b *countingBody) Read(p []byte) (int, error) { b.reads.Add(1); return b.Reader.Read(p) }
func (*countingBody) Close() error                 { return nil }

func TestHTTPUpgradePreservesDuplexBody(t *testing.T) {
	e := httpTestEngine(t, 1)
	body := &duplexBody{}
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 101, Header: http.Header{}, Body: body}, nil
	})
	rt := httpTestTransport(t, e, base, testHTTPPolicy(failClosed))
	req := httpTestRequest(t, "GET", "http://example.test")
	req.Header.Set("Upgrade", "websocket")
	res, err := rt.RoundTrip(req)
	if err != nil || res.Body != body {
		t.Fatalf("upgrade body replaced: %v", err)
	}
	if _, ok := res.Body.(io.ReadWriteCloser); !ok {
		t.Fatal("lost duplex capability")
	}
}

type duplexBody struct{ bytes.Buffer }

func (*duplexBody) Close() error { return nil }

func TestHTTPAmbiguousHeadersAndLateTrailers(t *testing.T) {
	e := httpTestEngine(t, 1)
	for _, h := range []http.Header{
		{"Content-Type": {"text/html", "application/json"}},
		{"Content-Type": {"text/html"}, "Content-Encoding": {"identity", "gzip"}},
	} {
		for _, policy := range []failurePolicy{failOpen, failClosed} {
			base := roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: h.Clone(), Body: io.NopCloser(strings.NewReader("untouched")), ContentLength: -1}, nil
			})
			rt := httpTestTransport(t, e, base, testHTTPPolicy(policy))
			res, err := rt.RoundTrip(httpTestRequest(t, "GET", "http://example.test"))
			if (err != nil) != (policy == failClosed) {
				t.Fatal("ambiguous headers ignored")
			}
			if res != nil {
				b, err := io.ReadAll(res.Body)
				_ = res.Body.Close()
				if err != nil || string(b) != "untouched" {
					t.Fatal("fallback changed body")
				}
			}
		}
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<p>prefix</p>")
		w.(http.Flusher).Flush()
		w.Header().Set(http.TrailerPrefix+"Digest", "original-digest")
	}))
	defer origin.Close()
	for _, policy := range []failurePolicy{failOpen, failClosed} {
		rt := httpTestTransport(t, e, http.DefaultTransport, testHTTPPolicy(policy))
		res, err := rt.RoundTrip(httpTestRequest(t, "GET", origin.URL))
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.ReadAll(res.Body)
		_ = res.Body.Close()
		if err == nil || rt.failed.Load() != 1 {
			t.Fatal("unexpected trailer escaped")
		}
	}
}
