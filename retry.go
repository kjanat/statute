package statute

import (
	"bytes"
	"io"
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
	statuses := m.RetryOnStatuses
	if maxAttempts < 1 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isRetryable(r) {
			next.ServeHTTP(w, r)
			return
		}

		bodyBytes, ok := bufferRetryBody(w, r, next)
		if !ok {
			return
		}

		for attempt := 1; attempt <= maxAttempts; attempt++ {
			if bodyBytes != nil {
				r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			}
			buf := newResponseBuffer()
			next.ServeHTTP(buf, r)
			if attempt == maxAttempts || !statusMatches(buf.status, statuses) {
				buf.replay(w)
				return
			}
		}
	})
}

// bufferRetryBody reads and buffers the request body so it can be replayed
// across attempts. ok is false when the caller must return without retrying:
// either buffering failed (a 400 was already written) or the body exceeded
// maxRetryBufferBytes (a single-shot pass was already served via next).
func bufferRetryBody(w http.ResponseWriter, r *http.Request, next http.Handler) (body []byte, ok bool) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, true
	}
	limited := io.LimitReader(r.Body, maxRetryBufferBytes+1)
	b, err := io.ReadAll(limited)
	if err != nil {
		_ = r.Body.Close()
		http.Error(w, "could not buffer request body for retry: "+err.Error(), http.StatusBadRequest)
		return nil, false
	}
	if len(b) > maxRetryBufferBytes {
		// Body too large to buffer; do a single-shot pass without retry.
		// r.Body is left open on purpose: the new body wraps it to supply
		// the bytes past the buffered prefix, and closing it here would
		// make those reads fail with ErrBodyReadAfterClose.
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(b), r.Body))
		next.ServeHTTP(w, r)
		return nil, false
	}
	// The whole body fit in the buffer (LimitReader hit EOF before the
	// cap), so the original stream is drained and safe to close.
	_ = r.Body.Close()
	return b, true
}

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
