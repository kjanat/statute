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
					for _, field := range []string{"If-None-Match", "If-Modified-Since", "Range", "If-Range"} {
						for key := range r.Header {
							if strings.EqualFold(key, field) {
								t.Fatalf("forwarded %s", key)
							}
						}
					}
					for _, field := range []string{"If-Match", "If-Unmodified-Since", "Accept-Language", "Accept-Encoding"} {
						if r.Header.Get(field) != original.Get(field) {
							t.Fatalf("changed %s", field)
						}
					}
					return &http.Response{StatusCode: 200, ContentLength: -1,
						Header: http.Header{"Content-Type": {media}, "Vary": {"Accept-Language", "Accept-Encoding"}},
						Body:   io.NopCloser(strings.NewReader(`<a class="rewrite">whole</a>`))}, nil
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
						return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {media}},
							Body: &countingBody{Reader: strings.NewReader("unread"), reads: &reads}}, nil
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
