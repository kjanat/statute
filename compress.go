package statute

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/kjanat/statute/resolved"
)

// compressHandler negotiates response compression based on the request's
// Accept-Encoding. Only gzip is implemented in this MVP; brotli is recognised
// but degrades to gzip when listed alongside it, otherwise to identity.
func compressHandler(algos []resolved.CompressAlgo, next http.Handler) http.Handler {
	wantGzip := false
	for _, a := range algos {
		if a == resolved.Gzip {
			wantGzip = true
		}
	}
	if !wantGzip {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
		w.Header().Del("Content-Length")
		gz := gzipPool.Get().(*gzip.Writer)
		gz.Reset(w)
		defer func() {
			gz.Close()
			gzipPool.Put(gz)
		}()
		gw := &gzipResponseWriter{ResponseWriter: w, w: gz}
		next.ServeHTTP(gw, r)
	})
}

var gzipPool = sync.Pool{
	New: func() any { return gzip.NewWriter(io.Discard) },
}

type gzipResponseWriter struct {
	http.ResponseWriter
	w io.Writer
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) { return g.w.Write(b) }
