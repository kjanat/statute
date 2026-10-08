package statute

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestETagTrailersWire(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		for _, late := range []bool{false, true} {
			for name, mws := range map[string][]Middleware{
				"etag":           {ETag()},
				"retry":          {Retry(2), ETag()},
				"inner-retry":    {ETag(), Retry(2)},
				"cache":          {Cache("1m"), ETag()},
				"inner-cache":    {ETag(), Cache("1m")},
				"compress":       {Compress(Gzip), ETag()},
				"inner-compress": {ETag(), Compress(Gzip)},
			} {
				t.Run(fmt.Sprintf("h2=%t/late=%t/%s", h2, late, name), func(t *testing.T) {
					var calls atomic.Int32
					producer := etagTrailerProducer(late, &calls)
					srv := httptest.NewUnstartedServer(chain(t, producer, mws...))
					srv.EnableHTTP2 = h2
					srv.StartTLS()
					defer srv.Close()
					for range 2 {
						resp, err := srv.Client().Get(srv.URL)
						if err != nil {
							t.Fatal(err)
						}
						assertETagTrailerResponse(t, resp, h2)
						if strings.Contains(name, "compress") && !resp.Uncompressed {
							t.Fatal("compression composition did not encode the response")
						}
					}
					if strings.Contains(name, "cache") && calls.Load() != 1 {
						t.Fatalf("cache did not reuse response: %d producer calls", calls.Load())
					}
				})
			}
		}
	}
}

func etagTrailerProducer(late bool, calls *atomic.Int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		if !late {
			w.Header().Set("Trailer", "X-Finished")
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, strings.Repeat("body", 256))
		key := "X-Finished"
		if late {
			key = http.TrailerPrefix + key
		}
		w.Header()[key] = []string{"done", "twice"}
	})
}

func assertETagTrailerResponse(t *testing.T, resp *http.Response, h2 bool) {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || string(body) != strings.Repeat("body", 256) || resp.StatusCode != 200 {
		t.Fatalf("body/status: %d len=%d err=%v", resp.StatusCode, len(body), err)
	}
	if (resp.ProtoMajor == 2) != h2 || resp.Header.Get("ETag") == "" ||
		resp.Header.Get("X-Finished") != "" || strings.Join(resp.Trailer.Values("X-Finished"), ",") != "done,twice" {
		t.Fatalf("protocol=%s headers=%v trailers=%v", resp.Proto, resp.Header, resp.Trailer)
	}
}

func TestETagBodylessTrailers(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		for _, condition := range []string{"", "If-None-Match", "If-Match"} {
			t.Run(method+"/"+condition, func(t *testing.T) {
				h := etagHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Trailer", "X-Finished, ETag")
					_, _ = io.WriteString(w, "body")
					w.Header().Set("X-Finished", "done")
					w.Header().Set("ETag", "origin")
					w.Header().Set(http.TrailerPrefix+"X-Late", "late")
				}))
				r := httptest.NewRequest(method, "/", nil)
				want := http.StatusOK
				switch condition {
				case "If-None-Match":
					r.Header.Set(condition, "*")
					want = http.StatusNotModified
				case "If-Match":
					r.Header.Set(condition, `"different"`)
					want = http.StatusPreconditionFailed
				}
				rec := runRequest(t, h, r)
				if rec.Code != want || rec.Header().Get("ETag") == "" || rec.Header().Get("ETag") == "origin" {
					t.Fatalf("status=%d headers=%v", rec.Code, rec.Header())
				}
				if method == http.MethodHead || condition != "" {
					assertNoETagTrailers(t, rec)
				}
			})
		}
	}
}

func assertNoETagTrailers(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Body.Len() != 0 || len(responseTrailerNames(rec.Header())) != 0 || rec.Header().Get("X-Finished") != "" {
		t.Fatalf("bodyless response retained trailers: %v body=%q", rec.Header(), rec.Body.String())
	}
}
