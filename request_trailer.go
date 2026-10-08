package statute

import (
	"io"
	"maps"
	"net/http"
	"net/http/httputil"
	"sync"
)

// preserveRequestTrailers runs before context wrappers can copy the transport's
// request. Some transports replace the Trailer map at EOF.
func preserveRequestTrailers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil || r.Body == http.NoBody || (r.ProtoMajor < 2 && len(r.TransferEncoding) == 0 && len(r.Trailer) == 0) {
			next.ServeHTTP(w, r)
			return
		}
		ctx := snapshotRetryTrailerNames(r.Context(), r.Trailer)
		forwarded := r.WithContext(ctx)
		forwarded.Trailer = announcedRetryTrailers(ctx)
		forwarded.Body = &requestTrailerBody{source: r, body: r.Body, trailers: forwarded.Trailer}
		next.ServeHTTP(w, forwarded)
	})
}

type requestTrailerBody struct {
	mu        sync.Mutex
	source    *http.Request
	body      io.ReadCloser
	trailers  http.Header
	published bool
	closed    bool
	failed    bool
}

func (b *requestTrailerBody) liveRetryTrailers() http.Header { return b.trailers }

func (b *requestTrailerBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if err != nil {
		b.mu.Lock()
		if err != io.EOF {
			b.failed = true
		}
		if err == io.EOF && !b.closed && !b.failed && !b.published {
			clear(b.trailers)
			maps.Copy(b.trailers, b.source.Trailer)
			b.published = true
		}
		b.mu.Unlock()
	}
	return n, err
}

func (b *requestTrailerBody) Close() error {
	// Do not serialize Close behind Read: transport Close may unblock Read.
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	return b.body.Close()
}

// Native forwarding owns its framing, without changing the incoming request.
// Explicit chunking also prevents HTTP/1 GET lookahead from reading trailers
// while the transport is still preparing its initial headers.
func forwardRequestTrailers(pr *httputil.ProxyRequest) {
	body := pr.Out.Body
	if body == nil && pr.In.ContentLength == 0 {
		body = pr.In.Body // ReverseProxy elides this body before Rewrite.
	}
	source, ok := body.(retryTrailerBody)
	if !ok {
		return
	}
	pr.Out.Body = body
	pr.Out.ContentLength = -1
	pr.Out.TransferEncoding = []string{"chunked"}
	pr.Out.Trailer = source.liveRetryTrailers()
}
