package statute

import (
	"bytes"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"

	"statute.kjanat.dev/resolved"
)

// maxRetryBufferBytes caps the request body size that retry will buffer in
// memory. Bodies larger than this skip retry — the handler streams the
// request straight through and responds with whatever the upstream returns.
// The cap exists because retry buffers the body to replay it on each attempt,
// and unbounded buffering is a memory denial-of-service vector for proxies
// that accept user uploads.
const maxRetryBufferBytes = 1 << 20 // 1 MiB

// retryHandler retries the wrapped handler up to max times when the response
// status matches one of the configured codes.
//
// Retry is skipped — and the request is forwarded as a single attempt — when
// any of the following is true:
//
//   - The request method is not idempotent (POST, PATCH, CONNECT, etc.).
//     Retrying these risks double-executing a side effect on the upstream.
//   - The request is gRPC (Content-Type starts with "application/grpc").
//     gRPC carries semantics — including streaming — that this naive retry
//     cannot observe. Retry is the gRPC layer's responsibility, not ours.
//   - The request advertises a streaming or upgraded protocol (WebSocket,
//     SSE via text/event-stream). Buffering would break the stream.
//   - The request body exceeds maxRetryBufferBytes. We cannot buffer it
//     without becoming a memory exhaustion target.
//
// In all other cases the body is buffered once and replayed for each
// attempt. The response is buffered until a non-retryable status arrives or
// the attempt budget is exhausted, then committed to the wire.
func retryHandler(m resolved.Middleware, next http.Handler) http.Handler {
	maxAttempts := m.RetryMax
	if maxAttempts < 1 {
		return next
	}
	budget := newResponseBufferBudget(m.ResponseBufferBudgetBytes)
	requestBudget := newResponseBufferBudget(m.RequestBufferBudgetBytes)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isRetryable(r) {
			next.ServeHTTP(w, r)
			return
		}

		if r.Body == nil || r.Body == http.NoBody {
			serveRetryAttempts(w, r, next, m, budget, nil)
			return
		}
		body := newRetryRequestBuffer(requestBudget)
		if body == nil {
			// Admission pressure leaves the original body untouched and
			// forwards the request as one attempt.
			next.ServeHTTP(w, r)
			return
		}
		defer body.release()
		r, ok := bufferRetryBody(w, r, next, body)
		if !ok {
			return
		}
		serveRetryAttempts(w, r, next, m, budget, body.body)
	})
}

func serveRetryAttempts(w http.ResponseWriter, r *http.Request, next http.Handler, m resolved.Middleware, budget *responseBufferBudget, body []byte) {
	for attempt := 1; attempt <= m.RetryMax; attempt++ {
		attemptRequest := r
		if body != nil {
			attemptRequest = r.WithContext(r.Context())
			attemptRequest.Body = io.NopCloser(bytes.NewReader(body))
			if len(r.Trailer) != 0 {
				attemptRequest.Body = &retryTrailerReadCloser{ReadCloser: attemptRequest.Body, trailers: r.Trailer}
			}
		}
		if !serveRetryAttempt(w, attemptRequest, next, m, budget, attempt == m.RetryMax) {
			return
		}
	}
}

func serveRetryAttempt(w http.ResponseWriter, r *http.Request, next http.Handler, m resolved.Middleware, budget *responseBufferBudget, last bool) bool {
	buf := newLimitedResponseBuffer(m.MaxResponseBodyBytes)
	buf.budget = budget
	defer buf.release()
	if !buf.render(next, r) {
		writeResponseLimitFailure(w, buf.failureStatus)
		return false
	}
	if last || !statusMatches(buf.status, m.RetryOnStatuses) {
		buf.replay(w)
		return false
	}
	return true
}

// bufferRetryBody reads into admitted storage for replay across attempts. False
// means a read error was answered with 400, or a single attempt already served
// the buffered prefix plus original stream after growth/size admission failed.
func bufferRetryBody(w http.ResponseWriter, r *http.Request, next http.Handler, b *retryRequestBuffer) (*http.Request, bool) {
	// Snapshot request fields and announced keys before body consumption.
	// Trailer values remain transport-owned until a read observes EOF.
	ctx := withRetryRequestBuffer(retryTrailerContext(r.Context(), r.Trailer), b)
	forwarded := r.WithContext(ctx)
	forwarded.Trailer = announcedRetryTrailers(ctx)
	complete, err := b.readFrom(r.Body)
	if err != nil {
		_ = r.Body.Close()
		http.Error(w, "could not buffer request body for retry: "+err.Error(), http.StatusBadRequest)
		return nil, false
	}
	if !complete {
		// Body too large or growth capacity unavailable: one attempt only.
		// The new body replays the buffered prefix then continues from the
		// original stream; its Close closes the original so the underlying
		// body is not leaked when the server/downstream closes r.Body.
		forwarded.Body = &multiReadCloser{
			r:    io.MultiReader(bytes.NewReader(b.body), r.Body),
			orig: r.Body, source: r, trailers: forwarded.Trailer,
		}
		next.ServeHTTP(w, forwarded)
		return nil, false
	}
	// The whole body fit and reached EOF, so the original stream is drained
	// and safe to close.
	_ = r.Body.Close()
	// Transport body readers can replace r.Trailer during Read. Capture the
	// final map after EOF so every replay includes the completed trailers.
	forwarded.Trailer = r.Trailer
	return forwarded.WithContext(snapshotRetryTrailerNames(ctx, r.Trailer)), true
}

// multiReadCloser concatenates a buffered prefix with the original request
// body and closes that original body on Close, so swapping it into r.Body
// does not leak the underlying stream.
type multiReadCloser struct {
	r        io.Reader
	orig     io.Closer
	source   *http.Request
	trailers http.Header
}

func (m *multiReadCloser) liveRetryTrailers() http.Header { return m.trailers }

// Read reads from the concatenated prefix+body reader.
func (m *multiReadCloser) Read(p []byte) (int, error) {
	n, err := m.r.Read(p)
	// Publish trailers only at EOF: Close may still mutate them earlier.
	// Keep map identity stable for shallow clones and Timeout producers.
	if err == io.EOF {
		clear(m.trailers)
		maps.Copy(m.trailers, m.source.Trailer)
	}
	return n, err
}

// Close closes the original underlying body.
func (m *multiReadCloser) Close() error { return m.orig.Close() }

// isRetryable returns true when the request meets the safety preconditions
// for retry. See retryHandler's doc comment for the full rationale.
func isRetryable(r *http.Request) bool {
	if !isIdempotent(r.Method) {
		return false
	}
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/grpc") {
		return false
	}
	if strings.HasPrefix(ct, "text/event-stream") {
		return false
	}
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	return true
}

func isIdempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete, http.MethodTrace:
		return true
	default:
		return false
	}
}

func statusMatches(status int, codes []int) bool {
	return slices.Contains(codes, status)
}
