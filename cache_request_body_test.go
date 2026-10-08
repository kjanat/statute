package statute

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCacheRequestBodyIsolation(t *testing.T) {
	for name, mws := range map[string][]Middleware{
		"cache":            {Cache("1h")},
		"cache-etag-retry": {Cache("1h"), ETag(), Retry(2)},
		"etag-retry-cache": {ETag(), Retry(2), Cache("1h")},
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			t.Run(name+"/"+method, func(t *testing.T) {
				calls := 0
				h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
					}
					w.Header().Set("X-Body", string(body))
					w.Header().Set("X-Call", fmt.Sprint(calls))
				}), mws...)
				for _, tc := range []struct {
					body   string
					length int64
					call   int
				}{
					{"", 0, 1}, {"a", 1, 2}, {"b", -1, 3}, {"", 0, 1},
				} {
					r := httptest.NewRequest(method, "/", nil)
					if tc.body != "" {
						r.Body = io.NopCloser(strings.NewReader(tc.body))
						r.ContentLength = tc.length
					}
					got := runRequest(t, h, r)
					if got.Header().Get("X-Body") != tc.body || got.Header().Get("X-Call") != fmt.Sprint(tc.call) {
						t.Fatalf("body=%q length=%d: headers=%v", tc.body, tc.length, got.Header())
					}
				}
			})
		}
	}
}

type cacheUnreadBody struct{ t *testing.T }

func (b *cacheUnreadBody) Read([]byte) (int, error) {
	b.t.Error("Cache read the request body")
	return 0, io.EOF
}
func (b *cacheUnreadBody) Close() error { b.t.Error("Cache closed the request body"); return nil }

func TestCacheBodyBypassPreservesOwnership(t *testing.T) {
	for _, tc := range []struct {
		name     string
		length   int64
		proto    int
		transfer bool
		trailer  bool
	}{
		{"known", 1, 1, false, false},
		{"unknown", -1, 2, false, false},
		{"http3-unknown", -1, 3, false, false},
		{"custom-http1-zero", 0, 1, false, false},
		{"transfer", 0, 2, true, false},
		{"trailer", 0, 2, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &cacheUnreadBody{t: t}
			calls := 0
			h := chain(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				calls++
				if r.Body != body || r.ContentLength != tc.length {
					t.Error("Cache changed body ownership or framing")
				}
			}), Cache("1h"))
			for range 2 {
				r := httptest.NewRequest(http.MethodGet, "/", nil)
				r.Body, r.ContentLength, r.ProtoMajor = body, tc.length, tc.proto
				if tc.transfer {
					r.TransferEncoding = []string{"chunked"}
				}
				if tc.trailer {
					r.Trailer = http.Header{"X-Selection": nil}
				}
				h.ServeHTTP(httptest.NewRecorder(), r)
			}
			if calls != 2 {
				t.Fatalf("body-dependent response stored: %d calls", calls)
			}
		})
	}
}
