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
// Accept-Encoding and the actual representation. Explicit coding exclusions
// are respected; Brotli wins quality ties between the supported codecs.
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
		prefs, valid := parseAcceptEncoding(r.Header)
		if !valid {
			stripUnacceptableRepresentation(ww.Header())
			ww.WriteHeader(http.StatusBadRequest)
			return
		}
		n := &compressionNegotiator{prefs: prefs, gzip: wantGzip, brotli: wantBrotli, transform: cacheControlAllows(r.Header, "no-transform")}
		serveCompressed(ww, r, next, n)
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

func serveCompressed(w http.ResponseWriter, r *http.Request, next http.Handler, n *compressionNegotiator) {
	cw := &compressResponseWriter{ResponseWriter: w, negotiation: n, head: r.Method == http.MethodHead, multiplexed: r.ProtoMajor >= 2}
	cw.buffered, _ = r.Context().Value(bufferedETagRenderKey{}).(bool)
	returned := false
	defer func() { cw.finish(returned) }()
	r = r.WithContext(context.WithValue(r.Context(), compressedRepresentationKey{}, n))
	next.ServeHTTP(cw, r)
	returned = true
}

func serveGzip(w http.ResponseWriter, r *http.Request, next http.Handler) {
	compressHandler([]resolved.CompressAlgo{resolved.Gzip}, next).ServeHTTP(w, r)
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
	negotiation     *compressionNegotiator
	head            bool
	buffered        bool
	rejected        bool
	hijacked        bool
	encoded         bool
	multiplexed     bool
	droppedTrailers headerNameSet
}

// Write forwards bytes to the underlying compressing writer.
func (c *compressResponseWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.WriteHeader(http.StatusOK)
	}
	if c.bodyless() {
		return 0, http.ErrBodyNotAllowed
	}
	if c.head || c.rejected {
		return len(b), nil
	}
	if c.w == nil {
		return c.ResponseWriter.Write(b)
	}
	return c.w.Write(b)
}

func (c *compressResponseWriter) WriteHeader(code int) {
	if code == http.StatusSwitchingProtocols && c.multiplexed {
		return
	}
	if c.status != 0 {
		return
	}
	if code >= 200 || code == http.StatusSwitchingProtocols {
		c.status = code
		c.startEncoding()
		code = c.status
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
	if c.bodyless() {
		return
	}
	coding, acceptable := c.negotiation.selectCoding(h, c.status)
	if !acceptable {
		c.rejected = true
		// Preserve denials and upstream failures for auth and Retry, without
		// sending their unacceptable body representation.
		if c.status < http.StatusBadRequest {
			c.status = http.StatusNotAcceptable
		}
		c.droppedTrailers = stripUnacceptableRepresentation(h)
		return
	}
	if coding == "" {
		return
	}
	c.coding = coding
	c.encoded = true
	c.droppedTrailers = stripCompressionTrailers(h)
	deleteHeaderFold(h, "Content-Length")
	for _, name := range []string{"Content-Encoding", headerContentMD5, headerDigest, headerContentDigest, headerReprDigest, "Accept-Ranges"} {
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
	if c.coding == codingGzip {
		c.gzip = gzipPool.Get().(*gzip.Writer)
		c.gzip.Reset(c.ResponseWriter)
		c.w = c.gzip
	} else {
		c.brotli = brotliPool.Get().(*brotli.Writer)
		c.brotli.Reset(c.ResponseWriter)
		c.w = c.brotli
	}
}

func (c *compressResponseWriter) finish(returned bool) {
	if returned && c.status == 0 && !c.hijacked {
		c.WriteHeader(http.StatusOK)
	}
	c.finishTrailers()
	if c.rejected {
		c.finishRejection()
	}
	c.finishCodecs(returned)
}

func (c *compressResponseWriter) finishCodecs(returned bool) {
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
	if c.buffered {
		return
	}
	if f, ok := c.w.(interface{ Flush() error }); ok && !c.bodyless() {
		_ = f.Flush()
	}
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
