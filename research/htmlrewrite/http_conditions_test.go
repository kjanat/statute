//go:build statute_htmlrewrite

package htmlrewrite

import (
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHTTPFullRepresentationRequest(t *testing.T) {
	e := httpTestEngine(t, 1)
	for _, media := range []string{"text/html", "application/json"} {
		for _, method := range []string{"GET", "HEAD"} {
			t.Run(method+"/"+media, func(t *testing.T) {
				req := httpTestRequest(t, method, "http://example.test")
				req.Header = http.Header{
					"If-None-Match": {`"old"`}, "If-Modified-Since": {"Wed, 21 Oct 2015 07:28:00 GMT"},
					"if-none-match": {`"also-old"`}, "range": {"bytes=10-20"},
					"Range": {"bytes=0-9"}, "If-Range": {`"old"`},
					"If-Match": {"*"}, "If-Unmodified-Since": {"Wed, 21 Oct 2015 07:28:00 GMT"},
					"Accept-Language": {"en"}, "Accept-Encoding": {"identity"},
				}
				original := req.Header.Clone()
				base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
					for _, field := range []string{"If-Match", "If-Unmodified-Since", "If-None-Match", "If-Modified-Since", "Range", "If-Range"} {
						for key := range r.Header {
							if strings.EqualFold(key, field) {
								t.Fatalf("forwarded %s", key)
							}
						}
					}
					for _, field := range []string{"Accept-Language", "Accept-Encoding"} {
						if r.Header.Get(field) != original.Get(field) {
							t.Fatalf("changed %s", field)
						}
					}
					return &http.Response{
						StatusCode: 200, ContentLength: -1,
						Header: http.Header{"Content-Type": {media}, "Vary": {"Accept-Language", "Accept-Encoding"}},
						Body:   io.NopCloser(strings.NewReader(`<a class="rewrite">whole</a>`)),
					}, nil
				})
				rt := httpTestTransport(t, e, base, testHTTPPolicy(failClosed))
				res, err := rt.RoundTrip(req)
				if err != nil {
					t.Fatal(err)
				}
				defer res.Body.Close()
				if !reflect.DeepEqual(req.Header, original) || !reflect.DeepEqual(res.Header.Values("Vary"), []string{"Accept-Language", "Accept-Encoding"}) {
					t.Fatal("mutated caller request or lost Vary")
				}
			})
		}
	}
}

func TestHTTPUnsolicitedRepresentations(t *testing.T) {
	e := httpTestEngine(t, 1)
	for _, status := range []int{206, 304} {
		for _, policy := range []failurePolicy{failClosed, failOpen} {
			for _, media := range []string{"text/html", "multipart/byteranges; boundary=a", "application/json", ""} {
				t.Run(fmt.Sprintf("%d/%d/%s", status, policy, media), func(t *testing.T) {
					var reads atomic.Int64
					base := roundTripFunc(func(*http.Request) (*http.Response, error) {
						return &http.Response{
							StatusCode: status, Header: http.Header{"Content-Type": {media}},
							Body: &countingBody{Reader: strings.NewReader("unread"), reads: &reads},
						}, nil
					})
					rt := httpTestTransport(t, e, base, testHTTPPolicy(policy))
					res, err := rt.RoundTrip(httpTestRequest(t, "GET", "http://example.test"))
					if err == nil || res != nil || reads.Load() != 0 || rt.rejected.Load() != 1 || rt.bypassed.Load() != 0 {
						t.Fatal("invalid full-representation response was delivered")
					}
				})
			}
		}
	}
}

func TestHTTPSelectedRepresentationConditions(t *testing.T) {
	e := httpTestEngine(t, 1)
	for _, tc := range []struct {
		field, value string
		html, plain  int
	}{
		{"If-None-Match", `"origin"`, 200, 304},
		{"If-None-Match", "*", 304, 304},
		{"If-Match", `"origin"`, 412, 200},
		{"If-Match", "*", 200, 200},
		{"If-Modified-Since", "Wed, 21 Oct 2015 07:28:00 GMT", 200, 304},
		{"If-Unmodified-Since", "Tue, 20 Oct 2015 07:28:00 GMT", 200, 412},
		{"If-None-Match", `"broken`, 400, 400},
	} {
		for _, method := range []string{"GET", "HEAD"} {
			for _, media := range []string{"text/html", "application/json"} {
				t.Run(method+"/"+media+"/"+tc.field+"/"+tc.value, func(t *testing.T) {
					var reads atomic.Int64
					base := roundTripFunc(func(*http.Request) (*http.Response, error) {
						return &http.Response{
							StatusCode: 200, ContentLength: -1,
							Header: http.Header{"Content-Type": {media}, "ETag": {`"origin"`}, "Last-Modified": {"Wed, 21 Oct 2015 07:28:00 GMT"}},
							Body:   &countingBody{Reader: strings.NewReader(`<a class="rewrite">body</a>`), reads: &reads},
						}, nil
					})
					rt := httpTestTransport(t, e, base, testHTTPPolicy(failClosed))
					req := httpTestRequest(t, method, "http://example.test")
					req.Header.Set(tc.field, tc.value)
					res, err := rt.RoundTrip(req)
					if err != nil {
						t.Fatal(err)
					}
					defer res.Body.Close()
					want := tc.plain
					if media == "text/html" {
						want = tc.html
					}
					if res.StatusCode != want {
						t.Fatalf("status=%d want=%d", res.StatusCode, want)
					}
					if want != 200 {
						body, err := io.ReadAll(res.Body)
						if err != nil || len(body) != 0 || reads.Load() != 0 || res.ContentLength != 0 {
							t.Fatal("conditional response consumed or delivered a body")
						}
						assertHTTPNoAdmission(t, e)
					}
				})
			}
		}
	}
}

func assertHTTPNoAdmission(t *testing.T, e *httpEngine) {
	t.Helper()
	e.mu.Lock()
	active := len(e.active)
	e.mu.Unlock()
	if active != 0 {
		t.Fatal("bodyless conditional response retained a rewrite instance")
	}
}
