//go:build statute_htmlrewrite

package htmlrewrite

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
)

func TestHTTPStatuteCompressionNegotiation(t *testing.T) {
	var calls atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Vary", "Accept-Encoding")
		_, _ = io.WriteString(w, `<a class="rewrite">page</a>`)
	}))
	defer origin.Close()
	endpoint := startHTTPStatute(t, origin.URL)
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}, Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	for _, path := range []string{"/negotiate-cached", "/cached-negotiate"} {
		before := calls.Load()
		for range 2 {
			for _, coding := range []string{"gzip", "br", "identity"} {
				assertHTTPNegotiatedRewrite(t, client, endpoint+path, coding)
			}
		}
		if got := calls.Load() - before; got != 3 {
			t.Fatalf("%s: variants did not reuse the cache: origin calls=%d", path, got)
		}
	}
	for _, path := range []string{"/negotiate-cached", "/cached-negotiate", "/etag-compress", "/compress-etag"} {
		for _, method := range []string{"GET", "HEAD"} {
			assertHTTPNegotiationRejection(t, client, endpoint+path, method)
		}
	}
}

func assertHTTPNegotiatedRewrite(t *testing.T, client *http.Client, endpoint, coding string) {
	t.Helper()
	req := httpTestRequest(t, "GET", endpoint)
	req.Header.Set("Accept-Encoding", coding)
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	want := coding
	if want == "identity" {
		want = ""
	}
	if res.StatusCode != 200 || res.Header.Get("Content-Encoding") != want {
		t.Fatalf("%s: status=%d headers=%v", coding, res.StatusCode, res.Header)
	}
	var reader io.Reader = res.Body
	switch coding {
	case "gzip":
		gz, err := gzip.NewReader(reader)
		if err != nil {
			t.Fatal(err)
		}
		defer gz.Close()
		reader = gz
	case "br":
		reader = brotli.NewReader(reader)
	}
	body, err := io.ReadAll(reader)
	if err != nil || strings.Count(string(body), "<em>inserted</em>") != 1 {
		t.Fatalf("negotiation/cache lost rewritten representation: %q %v", body, err)
	}
	if !strings.Contains(strings.Join(res.Header.Values("Vary"), ","), "Accept-Encoding") {
		t.Fatal("missing Accept-Encoding variance")
	}
}

func assertHTTPNegotiationRejection(t *testing.T, client *http.Client, endpoint, method string) {
	t.Helper()
	for _, conditional := range []bool{false, true} {
		req := httpTestRequest(t, method, endpoint)
		req.Header.Set("Accept-Encoding", "zstd, identity;q=0")
		if conditional {
			req.Header.Set("If-None-Match", "*")
		}
		wantStatus := http.StatusNotAcceptable
		if conditional && !strings.Contains(endpoint, "etag") {
			// Without an explicit ETag render, a downstream 304 is already
			// bodyless and has no payload coding for Compress to reject.
			wantStatus = http.StatusNotModified
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if err != nil || res.StatusCode != wantStatus || len(body) != 0 || res.Header.Get("ETag") != "" || res.Header.Get("Content-Encoding") != "" {
			t.Fatalf("%s %s conditional=%v: status=%d headers=%v body=%q err=%v", method, endpoint, conditional, res.StatusCode, res.Header, body, err)
		}
	}
}

func TestHTTPStatutePreservesAcceptedOriginCoding(t *testing.T) {
	// An opaque, unsupported coding must be delivered byte-for-byte; the
	// compression layer does not need a decoder or an encoder for passthrough.
	const opaque = "opaque origin zstd bytes"
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Encoding", "zstd")
		_, _ = io.WriteString(w, opaque)
	}))
	defer origin.Close()
	endpoint := startHTTPStatute(t, origin.URL)
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}, Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	for _, accept := range []string{"zstd,identity;q=0", "gzip"} {
		req := httpTestRequest(t, "GET", endpoint+"/open-compressed")
		req.Header.Set("Accept-Encoding", accept)
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if accept == "gzip" {
			if res.StatusCode != 406 || len(body) != 0 {
				t.Fatal("delivered forbidden origin coding")
			}
		} else if res.StatusCode != 200 || string(body) != opaque || res.Header.Get("Content-Encoding") != "zstd" {
			t.Fatalf("lost accepted origin coding: %d %v %q", res.StatusCode, res.Header, body)
		}
	}
}
