package statute

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"statute.kjanat.dev/resolved"
)

func TestCompressRepresentationTrailers(t *testing.T) {
	for _, coding := range []struct {
		name string
		algo resolved.CompressAlgo
	}{{"gzip", resolved.Gzip}, {"br", resolved.Brotli}} {
		for _, announced := range []bool{false, true} {
			for _, bypass := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/announced=%v/bypass=%v", coding.name, announced, bypass), func(t *testing.T) {
					base := compressionTrailerOrigin(announced, bypass)
					srv := httptest.NewServer(compressHandler([]resolved.CompressAlgo{coding.algo}, base))
					defer srv.Close()
					req := httptest.NewRequest("GET", srv.URL, nil)
					req.RequestURI = ""
					req.Header.Set("Accept-Encoding", coding.name)
					res, err := srv.Client().Do(req)
					if err != nil {
						t.Fatal(err)
					}
					_, err = io.Copy(io.Discard, res.Body)
					_ = res.Body.Close()
					if err != nil || res.Trailer.Get("X-Finished") != "yes" {
						t.Fatalf("response/trailers: %v %v", err, res.Trailer)
					}
					assertCompressionDigestTrailers(t, res.Trailer, bypass)
					if got := res.Header.Get("Content-Encoding"); (got == coding.name) == bypass {
						t.Fatalf("coding=%q, bypass=%v", got, bypass)
					}
				})
			}
		}
	}
}

func assertCompressionDigestTrailers(t *testing.T, trailers http.Header, bypass bool) {
	t.Helper()
	for _, name := range []string{"Content-Digest", "Repr-Digest"} {
		if got := trailers.Get(name); (got != "") != bypass {
			t.Fatalf("%s=%q, bypass=%v", name, got, bypass)
		}
		if _, present := trailers[name]; present && !bypass {
			t.Fatalf("stale %s announcement", name)
		}
	}
}

func compressionTrailerOrigin(announced, bypass bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		if bypass {
			w.Header().Set("Cache-Control", "no-transform")
		}
		if announced {
			w.Header().Add("Trailer", "Content-Digest, X-Finished")
			w.Header().Add("Trailer", "rEpR-dIgEsT")
		}
		_, _ = io.WriteString(w, "payload")
		w.(http.Flusher).Flush()
		prefix := ""
		if !announced {
			prefix = http.TrailerPrefix
		}
		w.Header().Set(prefix+"Content-Digest", "identity-digest")
		w.Header().Set(prefix+"Repr-Digest", "identity-repr-digest")
		w.Header().Set(prefix+"X-Finished", "yes")
	})
}

func TestCompressTrailerMetadataInBuffer(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header()["trailer"] = []string{"content-encoding, etag, repr-digest, X-Finished"}
		_, _ = io.WriteString(w, "payload")
		w.Header()["content-encoding"] = []string{"identity"}
		w.Header()["etag"] = []string{`"origin"`}
		w.Header()["repr-digest"] = []string{"identity-digest"}
		w.Header()["trailer:content-digest"] = []string{"late-identity-digest"}
		w.Header().Set("X-Finished", "yes")
	})
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	buf := newResponseBuffer()
	compressHandler([]resolved.CompressAlgo{resolved.Gzip}, base).ServeHTTP(buf, req)
	for _, name := range []string{"ETag", "Repr-Digest", "Trailer:Content-Digest"} {
		if values, present := cacheHeaderValues(buf.Header(), name); present {
			t.Fatalf("stale %s: %v", name, values)
		}
	}
	if values, _ := cacheHeaderValues(buf.Header(), "Content-Encoding"); len(values) != 1 || values[0] != "gzip" {
		t.Fatalf("lost generated coding: %v", values)
	}
	if buf.Header().Get("Trailer") != "X-Finished" || buf.Header().Get("X-Finished") != "yes" {
		t.Fatalf("lost unrelated trailer: %v", buf.Header())
	}
}
