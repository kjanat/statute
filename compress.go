package statute

import (
	"bufio"
	"compress/gzip"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"

	"statute.kjanat.dev/resolved"
)

// An inner body-derived validator covers bytes before this encoding stage.
type compressedRepresentationKey struct{}

// compressHandler negotiates response compression based on the request's
// Accept-Encoding. Brotli is preferred when both client and server advertise
// support; otherwise gzip is chosen. Identity (no compression) is the fallback.
func compressHandler(algos []resolved.CompressAlgo, next http.Handler) http.Handler {
	wantGzip, wantBrotli := compressionAlgorithms(algos)
	if !wantGzip && !wantBrotli {
		return next
	}
	vary := []headerOp{{op: resolved.MWAddResponseHeader, name: "Vary", value: "Accept-Encoding"}}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "" {
			next.ServeHTTP(w, r)
			return
		}
		// Identity and encoded output share one negotiation dimension.
		ww := &headerResponseWriter{ResponseWriter: w, ops: vary}
		ae := r.Header.Get("Accept-Encoding")
		switch {
		case wantBrotli && strings.Contains(ae, "br"):
			serveCompressed(ww, r, next, "br")
		case wantGzip && strings.Contains(ae, "gzip"):
			serveGzip(ww, r, next)
		default:
			next.ServeHTTP(ww, r)
		}
		ww.applyOps() // A normal empty response commits after the handler returns.
	})
}

func compressionAlgorithms(algos []resolved.CompressAlgo) (gzipEnabled, brotliEnabled bool) {
	for _, algo := range algos {
		switch algo {
		case resolved.Gzip:
			gzipEnabled = true
		case resolved.Brotli:
			brotliEnabled = true
		}
	}
	return
}

func serveCompressed(w http.ResponseWriter, r *http.Request, next http.Handler, coding string) {
	cw := &compressResponseWriter{ResponseWriter: w, coding: coding, head: r.Method == http.MethodHead, transform: cacheControlAllows(r.Header, "no-transform")}
	returned := false
	defer func() { cw.finish(returned) }()
	r = r.WithContext(context.WithValue(r.Context(), compressedRepresentationKey{}, true))
	next.ServeHTTP(cw, r)
	returned = true
}

func serveGzip(w http.ResponseWriter, r *http.Request, next http.Handler) {
	serveCompressed(w, r, next, "gzip")
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
	w               io.Writer
	gzip            *gzip.Writer
	brotli          *brotli.Writer
	status          int
	coding          string
	head            bool
	transform       bool
	hijacked        bool
	encoded         bool
	droppedTrailers []string
}

// Write forwards bytes to the underlying compressing writer.
func (c *compressResponseWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.WriteHeader(http.StatusOK)
	}
	if c.bodyless() {
		return 0, http.ErrBodyNotAllowed
	}
	if c.head {
		return len(b), nil
	}
	if c.w == nil {
		return c.ResponseWriter.Write(b)
	}
	return c.w.Write(b)
}

func (c *compressResponseWriter) WriteHeader(code int) {
	if c.status != 0 {
		return
	}
	if code >= 200 || code == http.StatusSwitchingProtocols {
		c.status = code
		c.startEncoding()
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *compressResponseWriter) bodyless() bool {
	return c.status == http.StatusSwitchingProtocols || c.status == http.StatusNoContent || c.status == http.StatusResetContent || c.status == http.StatusNotModified
}

func (c *compressResponseWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

func (c *compressResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(c.ResponseWriter).Hijack()
	if err == nil {
		c.hijacked = true
	}
	return conn, rw, err
}

// Final response headers decide whether a fresh coding is applicable.
func (c *compressResponseWriter) startEncoding() {
	h := c.Header()
	if !c.shouldEncode() {
		return
	}
	c.encoded = true
	c.droppedTrailers = stripCompressionTrailers(h)
	deleteHeaderFold(h, "Content-Length")
	for _, name := range []string{"Content-Encoding", "Content-MD5", "Digest", "Content-Digest", "Repr-Digest", "Accept-Ranges"} {
		deleteHeaderFold(h, name)
	}
	h.Set("Content-Encoding", c.coding)
	tags, _ := cacheHeaderValues(h, "ETag")
	if len(tags) == 1 && strings.HasPrefix(tags[0], `"`) {
		tag := tags[0]
		deleteHeaderFold(h, "ETag")
		h.Set("ETag", "W/"+tag)
	}
	if c.head {
		return
	}
	if c.coding == "gzip" {
		c.gzip = gzipPool.Get().(*gzip.Writer)
		c.gzip.Reset(c.ResponseWriter)
		c.w = c.gzip
	} else {
		c.brotli = brotliPool.Get().(*brotli.Writer)
		c.brotli.Reset(c.ResponseWriter)
		c.w = c.brotli
	}
}

func (c *compressResponseWriter) shouldEncode() bool {
	h := c.Header()
	encoding, _ := cacheHeaderValues(h, "Content-Encoding")
	_, partial := cacheHeaderValues(h, "Content-Range")
	encoded := strings.TrimSpace(strings.Join(encoding, ","))
	return !c.bodyless() && c.status != http.StatusPartialContent && !partial && c.transform && cacheControlAllows(h, "no-transform") && (encoded == "" || strings.EqualFold(encoded, "identity"))
}

func (c *compressResponseWriter) finish(returned bool) {
	if returned && c.status == 0 && !c.hijacked {
		c.WriteHeader(http.StatusOK)
	}
	c.finishTrailers()
	if c.gzip != nil {
		if !returned || c.hijacked {
			c.gzip.Reset(io.Discard)
		}
		_ = c.gzip.Close()
		gzipPool.Put(c.gzip)
	}
	if c.brotli != nil {
		if !returned || c.hijacked {
			c.brotli.Reset(io.Discard)
		}
		_ = c.brotli.Close()
		brotliPool.Put(c.brotli)
	}
}

// Flush flushes the underlying compression writer when it supports it,
// so streaming responses are not buffered indefinitely.
func (c *compressResponseWriter) Flush() {
	if c.status == 0 {
		c.WriteHeader(http.StatusOK)
	}
	if f, ok := c.w.(interface{ Flush() error }); ok && !c.bodyless() {
		_ = f.Flush()
	}
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
