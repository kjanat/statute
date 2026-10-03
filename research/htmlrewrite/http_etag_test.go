package htmlrewrite

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
)

func TestHTTPStatuteETagRepresentation(t *testing.T) {
	var badRender atomic.Bool
	var renders atomic.Uint64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			badRender.Store(true)
		}
		for _, name := range []string{"If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since", "Range", "If-Range"} {
			if r.Header.Get(name) != "" {
				badRender.Store(true)
			}
		}
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("ETag", `"origin"`)
		render := renders.Add(1)
		if render%2 == 0 {
			w.(http.Flusher).Flush()
		}
		_, _ = io.WriteString(w, `<a class="rewrite">`)
		if render%3 == 0 {
			w.(http.Flusher).Flush()
		}
		_, _ = io.WriteString(w, `page</a>`)
	}))
	defer origin.Close()
	endpoint := startHTTPStatute(t, origin.URL)
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}, Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	for _, path := range []string{"/etag", "/etag-cache", "/cache-etag", "/etag-compress", "/compress-etag", "/etag-compress-br", "/compress-etag-br"} {
		t.Run(path, func(t *testing.T) {
			req := httpTestRequest(t, "GET", endpoint+path)
			req.Header.Set("Accept-Encoding", "gzip, br")
			res, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(res.Body)
			_ = res.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			selected := body
			if res.Header.Get("Content-Encoding") == "gzip" {
				gz, err := gzip.NewReader(strings.NewReader(string(body)))
				if err != nil {
					t.Fatal(err)
				}
				selected, err = io.ReadAll(gz)
				_ = gz.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			if res.Header.Get("Content-Encoding") == "br" {
				selected, err = io.ReadAll(brotli.NewReader(strings.NewReader(string(body))))
				if err != nil {
					t.Fatal(err)
				}
			}
			if strings.Count(string(selected), "<em>inserted</em>") != 1 {
				t.Fatal("validator did not cover a single rewritten representation")
			}
			if strings.HasPrefix(path, "/etag-compress") {
				selected = body
			}
			sum := sha256.Sum256(selected)
			want := `"` + hex.EncodeToString(sum[:16]) + `"`
			if strings.HasPrefix(path, "/compress-etag") {
				want = "W/" + want
			}
			if res.Header.Get("ETag") != want {
				t.Fatalf("validator=%s want=%s", res.Header.Get("ETag"), want)
			}
			for _, method := range []string{"HEAD", "GET"} {
				assertHTTPETagCondition(t, client, endpoint+path, method, want, res.Header)
			}
		})
	}
	if badRender.Load() {
		t.Fatal("origin saw HEAD or a conditional/partial internal render")
	}
}

func assertHTTPETagCondition(t *testing.T, client *http.Client, endpoint, method, tag string, getHeader http.Header) {
	t.Helper()
	for _, conditional := range []bool{false, true} {
		req := httpTestRequest(t, method, endpoint)
		req.Header.Set("Accept-Encoding", "gzip, br")
		req.Header.Set("Range", "bytes=0-1")
		req.Header.Set("If-Range", `"origin"`)
		req.Header.Set("If-Modified-Since", "Wed, 21 Oct 2015 07:28:00 GMT")
		req.Header.Set("If-Unmodified-Since", "Wed, 21 Oct 2015 07:28:00 GMT")
		wantStatus := 200
		if conditional {
			req.Header.Set("If-None-Match", `"different", `+tag)
			wantStatus = 304
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if err != nil || res.StatusCode != wantStatus || res.Header.Get("ETag") != tag {
			t.Fatalf("%s conditional=%v: status=%d headers=%v error=%v", method, conditional, res.StatusCode, res.Header, err)
		}
		if (method == "HEAD" || conditional) && len(body) != 0 {
			t.Fatalf("%s/%d delivered rendered bytes", method, res.StatusCode)
		}
		if !conditional {
			for _, name := range []string{"Content-Type", "Content-Encoding", "Vary"} {
				if res.Header.Get(name) != getHeader.Get(name) {
					t.Fatalf("%s %s differs from GET: %v / %v", method, name, res.Header, getHeader)
				}
			}
			if !strings.Contains(endpoint, "/compress-etag") && res.Header.Get("Content-Length") != getHeader.Get("Content-Length") {
				t.Fatal("buffered GET/HEAD representation lengths differ")
			}
		}
	}
}
