//go:build statute_htmlrewrite

package htmlrewrite

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHTTPHeadRepresentation(t *testing.T) {
	e := httpTestEngine(t, 1)
	for _, media := range []string{"text/html", "application/json"} {
		t.Run(media, func(t *testing.T) {
			var reads atomic.Int64
			base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodHead || r.Header.Get("If-None-Match") != "" || r.Header.Get("Range") != "" {
					t.Fatal("HEAD must stay HEAD and request fresh metadata")
				}
				return &http.Response{StatusCode: 200, ContentLength: 7,
					Header: http.Header{"Content-Type": {media}, "Content-Length": {"7"}, "Etag": {`"origin"`}, "etag": {`"lower"`}, "Repr-Digest": {"origin"}},
					Body:   &countingBody{Reader: strings.NewReader("unread!"), reads: &reads}}, nil
			})
			rt := httpTestTransport(t, e, base, testHTTPPolicy(failClosed))
			req := httpTestRequest(t, "HEAD", "http://example.test")
			req.Header.Set("If-None-Match", `"origin"`)
			req.Header.Set("Range", "bytes=0-1")
			res, err := rt.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if media == "text/html" {
				if res.ContentLength != -1 || res.Header.Get("Content-Length") != "" || res.Header.Get("ETag") != "" || res.Header.Get("Repr-Digest") != "" {
					t.Fatal("HEAD advertised origin representation metadata")
				}
				if _, ok := res.Header["etag"]; ok {
					t.Fatal("noncanonical origin validator survived")
				}
				if b, err := io.ReadAll(res.Body); err != nil || len(b) != 0 {
					t.Fatal("HEAD produced content")
				}
			} else if res.ContentLength != 7 || res.Header.Get("ETag") != `"origin"` {
				t.Fatal("non-HTML metadata changed")
			}
			e.mu.Lock()
			active := len(e.active)
			e.mu.Unlock()
			if reads.Load() != 0 || active != 0 || rt.rewritten.Load() != 0 || req.Header.Get("If-None-Match") != `"origin"` {
				t.Fatal("HEAD consumed content, acquired rewrite state, or changed caller")
			}
		})
	}
}

func TestHTTPStatuteHeadMetadata(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("ETag", `"origin"`)
		w.Header().Set("Content-Length", "26")
		if r.Method != "HEAD" {
			_, _ = io.WriteString(w, `<a class="rewrite">one</a>`)
		}
	}))
	defer origin.Close()
	endpoint := startHTTPStatute(t, origin.URL)
	client := &http.Client{Timeout: testHTTPPolicy(failClosed).timeout}
	defer client.CloseIdleConnections()
	res, err := client.Head(endpoint + "/strict")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil || len(b) != 0 || res.StatusCode != 200 || res.Header.Get("Content-Length") != "" || res.Header.Get("ETag") != "" {
		t.Fatalf("HEAD status=%d headers=%v body=%q err=%v", res.StatusCode, res.Header, b, err)
	}
}
