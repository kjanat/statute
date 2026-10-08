//go:build statute_htmlrewrite && htmlrewrite_research

package htmlrewrite

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestHTTPDockerLiveRequestTrailersWithoutRetry(t *testing.T) {
	const input = `<a class="rewrite">page</a>`
	type observation struct {
		body, before, after, header string
		declared, chunked           bool
		err                         error
	}
	observations := make(chan observation, 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, declared := r.Trailer["X-Selection"]
		got := observation{
			before: r.Trailer.Get("X-Selection"), header: r.Header.Get("X-Selection"),
			declared: declared, chunked: slices.Contains(r.TransferEncoding, "chunked"),
		}
		body, err := io.ReadAll(r.Body)
		got.body, got.err, got.after = string(body), err, r.Trailer.Get("X-Selection")
		if lateHeader := r.Header.Get("X-Selection"); lateHeader != "" {
			got.header = lateHeader
		}
		observations <- got
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, input)
	}))
	defer origin.Close()
	// Both routes use the same native pool without Retry. Only rewrite.test
	// adds the research transport, which deep-clones the outgoing request.
	endpoint, _ := startDockerRewriteStatute(t, origin.URL)
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}, Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	for _, tc := range []struct {
		name, host string
		rewrite    bool
	}{
		{"ordinary proxy control", "plain.test", false},
		{"rewrite deep clone", "rewrite.test", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, endpoint, strings.NewReader("payload"))
			if err != nil {
				t.Fatal(err)
			}
			req.ContentLength = -1
			req.Host = tc.host
			req.Trailer = http.Header{"X-Selection": {"selected"}}
			res, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(res.Body)
			_ = res.Body.Close()
			if err != nil || res.StatusCode != http.StatusOK {
				t.Fatalf("response: status=%d body=%q error=%v", res.StatusCode, body, err)
			}
			if tc.rewrite {
				if strings.Count(string(body), "<em>inserted</em>") != 1 {
					t.Fatalf("rewrite did not execute: %q", body)
				}
			} else if string(body) != input {
				t.Fatalf("control response changed: %q", body)
			}
			select {
			case got := <-observations:
				if got.err != nil || got.body != "payload" || !got.declared || !got.chunked ||
					got.before != "" || got.after != "selected" || got.header != "" {
					t.Fatalf("upstream trailer observation: %+v", got)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("origin did not report request trailers")
			}
		})
	}
}
