package statute

import (
	"fmt"
	"html"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCacheRequestSpecificSelection(t *testing.T) {
	for _, field := range []struct{ name, value string }{
		{"If-Match", `"current"`},
		{"If-None-Match", `"current"`},
		{"If-Modified-Since", "Wed, 21 Oct 2015 07:28:00 GMT"},
		{"If-Unmodified-Since", "Wed, 21 Oct 2015 07:28:00 GMT"},
		{"Range", "bytes=0-1"},
		{"If-Range", `"current"`},
		{"Cache-Control", "no-transform"},
		{"cache-control", `ext="a,b", No-Transform`},
		{"Cache-Control", "no-cache"},
		{"Cache-Control", `ext="invalid`},
	} {
		t.Run(field.name+"/"+field.value, func(t *testing.T) {
			for _, retryFirst := range []bool{false, true} {
				calls := 0
				base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls++
					_, _ = fmt.Fprintf(w, "response %d", calls)
				})
				mws := []Middleware{Cache("1h"), Retry(2, OnStatus(503))}
				if retryFirst {
					mws[0], mws[1] = mws[1], mws[0]
				}
				h := chain(t, base, mws...)
				runRequest(t, h, httptest.NewRequest("GET", "/", nil))
				req := httptest.NewRequest("GET", "/", nil)
				req.Header[field.name] = []string{field.value}
				if got := runRequest(t, h, req); got.Body.String() != "response 2" {
					t.Fatalf("request policy bypassed: %s", got.Body.String())
				}
				if got := runRequest(t, h, httptest.NewRequest("GET", "/", nil)); got.Body.String() != "response 1" || calls != 2 {
					t.Fatal("special request replaced or evicted the unconditional entry")
				}
			}
		})
	}
}

func TestCacheVarySelection(t *testing.T) {
	calls := 0
	h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Add("Vary", "Accept-Language")
		w.Header().Add("vary", "accept-encoding, ACCEPT-LANGUAGE")
		_, _ = fmt.Fprintf(w, "%s/%s", html.EscapeString(r.Header.Get("Accept-Language")), html.EscapeString(r.Header.Get("Accept-Encoding")))
	}), Cache("1h"))
	for _, value := range []struct{ language, encoding string }{{"en", "identity"}, {"fr", "identity"}, {"en", "gzip"}, {"en", "identity"}, {"fr", "identity"}, {"en", "gzip"}} {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Accept-Language", value.language)
		req.Header.Set("Accept-Encoding", value.encoding)
		if got := runRequest(t, h, req); got.Body.String() != value.language+"/"+value.encoding {
			t.Fatalf("wrong variant: %s", got.Body.String())
		}
	}
	if calls != 3 {
		t.Fatalf("variant reuse: got %d origin calls, want 3", calls)
	}
}

func TestCacheReplayOwnsHeaderValues(t *testing.T) {
	h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		values := make([]string, 1, 8)
		values[0] = "original"
		w.Header()["X-Values"] = values
		_, _ = fmt.Fprint(w, "body")
	}), Cache("1h"))
	first := runRequest(t, h, httptest.NewRequest("GET", "/", nil))
	first.Header()["X-Values"][0] = "changed by an outer writer"
	second := runRequest(t, h, httptest.NewRequest("GET", "/", nil))
	if second.Header().Get("X-Values") != "original" {
		t.Fatal("a response writer mutated the shared cache entry")
	}
}

func TestCacheUnusableRepresentations(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		header http.Header
	}{
		{"partial", 206, nil},
		{"content range", 200, http.Header{"content-range": {"bytes 0-1/5"}}},
		{"vary star", 200, http.Header{"Vary": {"Accept-Language", "*"}}},
		{"invalid vary", 200, http.Header{"Vary": {"Accept-Language, bad:field"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				maps.Copy(w.Header(), tc.header)
				w.WriteHeader(tc.status)
			}), Cache("1h"))
			for range 2 {
				if got := runRequest(t, h, httptest.NewRequest("GET", "/", nil)); got.Code != tc.status {
					t.Fatal("cache altered delivery")
				}
			}
			if calls != 2 {
				t.Fatal("unusable representation was stored")
			}
		})
	}
}

func TestCacheHoistedVary(t *testing.T) {
	for _, removeOrigin := range []bool{false, true} {
		calls := 0
		base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.Header().Set("Vary", "Accept-Language")
			_, _ = fmt.Fprint(w, html.EscapeString(r.Header.Get("Accept-Language")+r.Header.Get("X-Variant")))
		})
		mws := []Middleware{Cache("1h"), AddResponseHeader("Vary", "X-Variant")}
		if removeOrigin {
			mws = []Middleware{RemoveResponseHeader("Vary"), Cache("1h"), SetResponseHeader("Vary", "X-Variant")}
		}
		h := chain(t, base, mws...)
		for _, variant := range []string{"en/a", "fr/a", "en/b", "en/a", "fr/a", "en/b"} {
			language, extra, _ := strings.Cut(variant, "/")
			req := httptest.NewRequest("GET", "/", nil)
			req.Header.Set("Accept-Language", language)
			req.Header.Set("X-Variant", extra)
			if got := runRequest(t, h, req); got.Body.String() != language+extra {
				t.Fatalf("hoisted Vary lost: %s", got.Body.String())
			}
		}
		if calls != 3 {
			t.Fatalf("hoisted variants were not reused: %d", calls)
		}
	}
}

func TestCacheVaryRequestSnapshot(t *testing.T) {
	calls := 0
	h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Vary", "X-Variant")
		_, _ = fmt.Fprint(w, html.EscapeString(fmt.Sprint(r.Header["X-Variant"])))
		// Application code must not mutate the cache's request-key snapshot.
		r.Header.Set("X-Variant", "changed")
	}), Cache("1h"))
	for _, values := range [][]string{nil, {""}, {"a", "b"}, {"b", "a"}, nil, {""}, {"a", "b"}, {"b", "a"}} {
		req := httptest.NewRequest("GET", "/", nil)
		if values != nil {
			req.Header["X-Variant"] = values
		}
		if got := runRequest(t, h, req); got.Body.String() != fmt.Sprint(values) {
			t.Fatalf("variant mismatch: %s", got.Body.String())
		}
	}
	if calls != 4 {
		t.Fatalf("mutable/absent/multivalue keys lost: calls=%d", calls)
	}
}

func TestCacheVariantExpiryAndSchemaChange(t *testing.T) {
	c := newTTLCache(time.Hour)
	key := cacheKey{target: "key"}
	a := http.Header{"Accept-Language": {"en"}, "X-Variant": {"a"}}
	b := http.Header{"Accept-Language": {"fr"}, "X-Variant": {"b"}}
	one := publishCacheTestEntry(t, c, key, a, []string{"accept-language"}, "one")
	two := publishCacheTestEntry(t, c, key, b, []string{"accept-language"}, "two")
	one.expires = time.Now().Add(-time.Second)
	if getCacheTestEntry(c, key, a) != nil || getCacheTestEntry(c, key, b) != two {
		t.Fatal("expiring one variant invalidated its live sibling")
	}
	three := publishCacheTestEntry(t, c, key, a, []string{"x-variant"}, "three")
	if getCacheTestEntry(c, key, b) != nil || getCacheTestEntry(c, key, a) != three {
		t.Fatal("changed Vary schema retained incompatible entries")
	}
	one = publishCacheTestEntry(t, c, key, a, []string{"x-variant"}, "one")
	if getCacheTestEntry(c, key, a) != one || c.count != 1 {
		t.Fatal("replacing the same variant retained old data")
	}
}
