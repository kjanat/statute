package statute

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"

	"statute.kjanat.dev/resolved"
)

// compressHandler negotiates response compression based on the request's
// Accept-Encoding. Brotli is preferred when both client and server advertise
// support; otherwise gzip is chosen. Identity (no compression) is the fallback.
func compressHandler(algos []resolved.CompressAlgo, next http.Handler) http.Handler {
	wantGzip, wantBrotli := false, false
	for _, a := range algos {
		switch a {
		case resolved.Gzip:
			wantGzip = true
		case resolved.Brotli:
			wantBrotli = true
		}
	}
	if !wantGzip && !wantBrotli {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ae := r.Header.Get("Accept-Encoding")
		switch {
		case wantBrotli && strings.Contains(ae, "br"):
			serveBrotli(w, r, next)
		case wantGzip && strings.Contains(ae, "gzip"):
			serveGzip(w, r, next)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func serveGzip(w http.ResponseWriter, r *http.Request, next http.Handler) {
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Add("Vary", "Accept-Encoding")
	w.Header().Del("Content-Length")
	gz := gzipPool.Get().(*gzip.Writer)
	gz.Reset(w)
	defer func() {
		_ = gz.Close()
		gzipPool.Put(gz)
	}()
	next.ServeHTTP(&compressResponseWriter{ResponseWriter: w, w: gz}, r)
}

func serveBrotli(w http.ResponseWriter, r *http.Request, next http.Handler) {
	w.Header().Set("Content-Encoding", "br")
	w.Header().Add("Vary", "Accept-Encoding")
	w.Header().Del("Content-Length")
	br := brotliPool.Get().(*brotli.Writer)
	br.Reset(w)
	defer func() {
		_ = br.Close()
		brotliPool.Put(br)
	}()
	next.ServeHTTP(&compressResponseWriter{ResponseWriter: w, w: br}, r)
}

var (
	gzipPool = sync.Pool{
		New: func() any { return gzip.NewWriter(io.Discard) },
	}
	brotliPool = sync.Pool{
		New: func() any { return brotli.NewWriterLevel(io.Discard, brotli.DefaultCompression) },
	}
)

// compressResponseWriter forwards Write into the compressor while preserving
// access to the underlying ResponseWriter for headers and status. Flush is
// propagated through the compressor when supported.
type compressResponseWriter struct {
	http.ResponseWriter
	w io.Writer
}

// Write forwards bytes to the underlying compressing writer.
func (c *compressResponseWriter) Write(b []byte) (int, error) { return c.w.Write(b) }

// Flush flushes the underlying compression writer when it supports it,
// so streaming responses are not buffered indefinitely.
func (c *compressResponseWriter) Flush() {
	if f, ok := c.w.(interface{ Flush() error }); ok {
		_ = f.Flush()
	}
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
