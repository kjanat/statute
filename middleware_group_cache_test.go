package statute

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListenerMiddlewareCachesStayIndependent(t *testing.T) {
	for _, nested := range []bool{false, true} {
		calls := 0
		selection := Middleware(OnListeners([]string{"private"}, ReplaceEscapedPath("/private")))
		if nested {
			selection = InOrder(selection)
		}
		h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.Header().Set("Content-Type", "text/plain")
			// #nosec G705 -- Plain-text fixture reports the selected route path.
			_, _ = io.WriteString(w, r.URL.Path)
		}), Cache("1h"), selection)
		for _, name := range []string{"private", "public", "private", "public", ""} {
			r := httptest.NewRequest(http.MethodGet, "http://same.example/original", nil)
			w := httptest.NewRecorder()
			namedListener(name, h).ServeHTTP(w, r)
			want := "/original"
			if name == "private" {
				want = "/private"
			}
			if w.Body.String() != want {
				t.Fatalf("listener=%q nested=%t body=%q want=%q", name, nested, w.Body.String(), want)
			}
		}
		if calls != 3 {
			t.Fatalf("listener caches did not reuse their own entries: calls=%d", calls)
		}
	}
}

func TestGroupedCredentialsBypassSharedCache(t *testing.T) {
	for _, header := range []string{"Authorization", "Cookie"} {
		for _, add := range []bool{false, true} {
			var writer Middleware = SetRequestHeader(header, "private")
			if add {
				writer = AddRequestHeader(header, "private")
			}
			for _, ordered := range []bool{false, true} {
				t.Run(header+map[bool]string{false: "/set", true: "/add"}[add]+map[bool]string{false: "/listener", true: "/ordered"}[ordered], func(t *testing.T) {
					group := writer
					if ordered {
						group = InOrder(Cache("1h"), writer)
					}
					calls := 0
					h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls++
						w.Header().Set("Cache-Control", "public, max-age=3600")
						w.Header().Set("Content-Type", "text/plain")
						// #nosec G705 -- Plain-text fixture exposes which credential branch served the request.
						_, _ = w.Write([]byte(r.Header.Get(header)))
					}), Cache("1h"), OnListeners([]string{"private"}, group))
					for _, name := range []string{"private", "public", "private"} {
						r := httptest.NewRequest(http.MethodGet, "http://same.example/", nil)
						r = r.WithContext(context.WithValue(r.Context(), listenerNameKey{}, name))
						w := httptest.NewRecorder()
						h.ServeHTTP(w, r)
						want := ""
						if name == "private" {
							want = "private"
						}
						if w.Body.String() != want {
							t.Fatalf("listener %s received %q, want %q", name, w.Body.String(), want)
						}
					}
					if calls != 3 {
						t.Fatalf("credential route reused cache: calls=%d", calls)
					}
				})
			}
		}
	}
}
