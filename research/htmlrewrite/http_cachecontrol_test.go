//go:build statute_htmlrewrite

package htmlrewrite

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHTTPCacheControlGrammar(t *testing.T) {
	for _, tc := range []struct {
		value string
		allow bool
	}{
		{"", true},
		{"public, max-age=60", true},
		{", ,\t", true},
		{`extension="a, no-transform, b"`, true},
		{`extension="a\", no-transform, b"`, true},
		{`extension="a\\", no-transform`, false},
		{`extension=no-transform`, true},
		{`extension="no-transform"`, true},
		{"public,\tNo-Transform", false},
		{`no-transform="ignored"`, false},
		{`no-transform-extra`, true},
		{`extension="unterminated`, false},
		{`extension="quoted"junk`, false},
		{`extension=`, false},
		{"public no-transform", false},
		{"extension=\"control\x01\"", false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			if got := cacheControlPermitsTransform(http.Header{"cache-control": {tc.value}}); got != tc.allow {
				t.Fatalf("permits transform = %v, want %v", got, tc.allow)
			}
		})
	}
	h := http.Header{"Cache-Control": {"public"}, "cAcHe-CoNtRoL": {`extension="a,b"`, "NO-TRANSFORM"}}
	if cacheControlPermitsTransform(h) {
		t.Fatal("lost no-transform in a repeated/noncanonical field")
	}
}

func TestHTTPNoTransformPolicy(t *testing.T) {
	e := httpTestEngine(t, 1)
	const original = `<a class="rewrite">original</a>`
	for _, source := range []string{"request", "response"} {
		for _, policy := range []failurePolicy{failOpen, failClosed} {
			for _, value := range []string{"no-transform", `extension="unterminated`} {
				t.Run(fmt.Sprintf("%s/%s/%d", source, value, policy), func(t *testing.T) {
					var reads atomic.Int64
					req := httpTestRequest(t, "GET", "http://example.test")
					if source == "request" {
						req.Header["cache-control"] = []string{"public", value}
					}
					base := roundTripFunc(func(*http.Request) (*http.Response, error) {
						h := http.Header{"Content-Type": {"text/html"}, "Etag": {`"original"`}}
						if source == "response" {
							h["cAcHe-CoNtRoL"] = []string{"public", value}
						}
						return &http.Response{StatusCode: 200, Header: h, ContentLength: -1,
							Body: &countingBody{Reader: strings.NewReader(original), reads: &reads}}, nil
					})
					rt := httpTestTransport(t, e, base, testHTTPPolicy(policy))
					res, err := rt.RoundTrip(req)
					if reads.Load() != 0 || rt.rewritten.Load() != 0 {
						t.Fatal("forbidden HTML was consumed or admitted")
					}
					if policy == failClosed {
						if err == nil || res != nil || rt.rejected.Load() != 1 {
							t.Fatal("closed route did not reject")
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					defer res.Body.Close()
					b, err := io.ReadAll(res.Body)
					if err != nil || string(b) != original || res.Header.Get("ETag") != `"original"` ||
						res.Header.Get("Cache-Control") != "no-store" || rt.bypassed.Load() != 1 {
						t.Fatal("open route did not preserve original representation with no-store")
					}
				})
			}
		}
	}
}

func FuzzHTTPCacheControl(f *testing.F) {
	for _, seed := range []string{"public", `extension="a, no-transform, b"`, `extension="a\"b"`, "no-transform"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		h := http.Header{"Cache-Control": {value, "no-transform"}}
		if cacheControlPermitsTransform(h) {
			t.Fatal("another field hid no-transform")
		}
		if cacheFieldPermitsTransform(value) && cacheFieldPermitsTransform(value+", no-transform") {
			t.Fatal("appended no-transform was lost")
		}
	})
}
