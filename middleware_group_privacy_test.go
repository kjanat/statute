package statute

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGroupedHeaderEditsPreserveOriginCachePolicy(t *testing.T) {
	cases := []struct {
		name, header, value string
		edit                Middleware
		calls               int
	}{
		{"private removal", "Cache-Control", "private, no-store", RemoveResponseHeader("Cache-Control"), 2},
		{"private overwrite", "Cache-Control", "private", SetResponseHeader("Cache-Control", "public, max-age=3600"), 2},
		{"no-cache removal", "Cache-Control", "no-cache", RemoveResponseHeader("Cache-Control"), 2},
		{"cookie removal", "Set-Cookie", "session=private", RemoveResponseHeader("Set-Cookie"), 2},
		{"expired freshness", "Cache-Control", "max-age=0", RemoveResponseHeader("Cache-Control"), 2},
		{"partial metadata", "Content-Range", "bytes 0-3/100", RemoveResponseHeader("Content-Range"), 2},
		{"wildcard variance", "Vary", "*", RemoveResponseHeader("Vary"), 2},
		{"public control", "Cache-Control", "public, max-age=3600", SetResponseHeader("X-Policy", "yes"), 1},
	}
	for _, tc := range cases {
		for _, buffer := range []Middleware{InOrder(), ETag(), Retry(2), Timeout("1s")} {
			for _, conditional := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%T/listener=%t", tc.name, buffer, conditional), func(t *testing.T) {
					calls := 0
					group := Middleware(InOrder(InOrder(tc.edit)))
					if conditional {
						group = OnListeners([]string{"private"}, group)
					}
					h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						calls++
						w.Header().Set(tc.header, tc.value)
						w.Header().Set("Content-Type", "text/plain")
						_, _ = fmt.Fprintf(w, "response-%d", calls)
					}), Cache("1h"), buffer, group)
					for request := range 2 {
						w := httptest.NewRecorder()
						namedListener("private", h).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
						if w.Code != http.StatusOK {
							t.Fatal(w.Code)
						}
						if want := fmt.Sprintf("response-%d", min(request+1, tc.calls)); w.Body.String() != want {
							t.Fatalf("received body=%q want=%q", w.Body.String(), want)
						}
					}
					if calls != tc.calls {
						t.Fatalf("producer calls=%d want=%d", calls, tc.calls)
					}
				})
			}
		}
	}
}

func TestGroupedHeaderEditsCannotEraseOriginVariance(t *testing.T) {
	for _, edit := range []Middleware{RemoveResponseHeader("Vary"), SetResponseHeader("Vary", "Accept-Language"), AddResponseHeader("Vary", "Accept-Language")} {
		h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Vary", "X-Client")
			w.Header().Set("Content-Type", "text/plain")
			// #nosec G705 -- Plain-text fixture identifies the requested variant.
			_, _ = fmt.Fprint(w, r.Header.Get("X-Client"))
		}), Cache("1h"), InOrder(edit))
		for _, client := range []string{"first", "second", "first"} {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set("X-Client", client)
			w := runRequest(t, h, r)
			if w.Body.String() != client {
				t.Fatalf("client=%s received=%q", client, w.Body.String())
			}
		}
	}
}
