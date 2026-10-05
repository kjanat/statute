//go:build statute_htmlrewrite

package htmlrewrite

import (
	"compress/gzip"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPStatuteWarmCacheNoTransform(t *testing.T) {
	for _, path := range []string{"/cached", "/bypass-cached"} {
		t.Run(path, func(t *testing.T) {
			var calls atomic.Int64
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/html")
				_, _ = io.WriteString(w, `<a class="rewrite">page</a>`)
			}))
			defer origin.Close()
			endpoint := startHTTPStatute(t, origin.URL)
			client := &http.Client{Timeout: 5 * time.Second}
			defer client.CloseIdleConnections()
			for i, policy := range []string{"", "no-transform", "", `extension="no-transform"`} {
				req := httpTestRequest(t, "GET", endpoint+path)
				if policy != "" {
					req.Header.Set("Cache-Control", policy)
				}
				res, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(res.Body)
				_ = res.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				wantStatus := 200
				if i == 1 && path == "/cached" {
					wantStatus = 502
				}
				if res.StatusCode != wantStatus || strings.Contains(string(body), "<em>inserted</em>") != (i != 1) {
					t.Fatalf("request policy %q: status=%d body=%s", policy, res.StatusCode, body)
				}
			}
			if calls.Load() != 2 {
				t.Fatalf("cache bypass/reuse: %d origin calls, want 2", calls.Load())
			}
		})
	}
}

func TestHTTPStatuteCacheVaryCompression(t *testing.T) {
	for _, path := range []string{"/vary-cached", "/vary-compressed", "/compressed-vary"} {
		t.Run(path, func(t *testing.T) {
			var calls atomic.Int64
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/html")
				w.Header().Add("Vary", "Accept-Language")
				_, _ = io.WriteString(w, `<a class="rewrite">`+html.EscapeString(r.Header.Get("Accept-Language"))+`</a>`)
			}))
			defer origin.Close()
			endpoint := startHTTPStatute(t, origin.URL)
			client := &http.Client{Transport: &http.Transport{DisableCompression: true}, Timeout: 5 * time.Second}
			defer client.CloseIdleConnections()
			for range 2 {
				for _, variant := range []struct{ language, encoding string }{{"en", "identity"}, {"fr", "identity"}, {"en", "gzip"}, {"fr", "gzip"}} {
					assertHTTPVariant(t, client, endpoint+path, variant.language, variant.encoding, path != "/vary-cached")
				}
			}
			wantCalls := int64(2)
			if path == "/vary-compressed" {
				wantCalls = 4 // Encoded variants are stored, not decoded bodies.
			}
			if calls.Load() != wantCalls {
				t.Fatalf("cached variants: calls=%d want=%d", calls.Load(), wantCalls)
			}
		})
	}
}

func assertHTTPVariant(t *testing.T, client *http.Client, endpoint, language, encoding string, compressed bool) {
	t.Helper()
	req := httpTestRequest(t, "GET", endpoint)
	req.Header.Set("Accept-Language", language)
	req.Header.Set("Accept-Encoding", encoding)
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var reader io.Reader = res.Body
	if compressed && encoding == "gzip" {
		if res.Header.Get("Content-Encoding") != "gzip" {
			t.Fatal("gzip variant missing encoding")
		}
		gz, err := gzip.NewReader(reader)
		if err != nil {
			t.Fatal(err)
		}
		defer gz.Close()
		reader = gz
	} else if res.Header.Get("Content-Encoding") != "" {
		t.Fatal("encoded variant leaked into identity response")
	}
	body, err := io.ReadAll(reader)
	if err != nil || !strings.Contains(string(body), ">"+language+"<em>inserted</em>") || strings.Count(string(body), "<em>inserted</em>") != 1 {
		t.Fatalf("wrong or repeated transformation: %s, %v", body, err)
	}
}
