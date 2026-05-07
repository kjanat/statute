package statute

import (
	"bytes"
	"io"
	"net/http"

	"github.com/kjanat/statute/resolved"
)

// retryHandler retries the wrapped handler up to max times when the response
// status matches one of the configured codes.
//
// Only idempotent methods are retried. Non-idempotent methods (POST, PATCH,
// CONNECT) are passed through with no retry, because retrying them risks
// double-executing a side effect on the upstream.
//
// When the request has a body, the body is buffered into memory so it can be
// replayed for each attempt. This is unsuitable for very large uploads; in
// practice retry should not be configured on routes that accept large bodies.
func retryHandler(m resolved.Middleware, next http.Handler) http.Handler {
	max := m.RetryMax
	statuses := m.RetryOnStatuses
	if max < 1 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isIdempotent(r.Method) {
			next.ServeHTTP(w, r)
			return
		}

		var bodyBytes []byte
		if r.Body != nil && r.Body != http.NoBody {
			b, err := io.ReadAll(r.Body)
			_ = r.Body.Close()
			if err != nil {
				http.Error(w, "could not buffer request body for retry: "+err.Error(), http.StatusBadRequest)
				return
			}
			bodyBytes = b
		}

		for attempt := 1; attempt <= max; attempt++ {
			if bodyBytes != nil {
				r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			}
			buf := newResponseBuffer()
			next.ServeHTTP(buf, r)
			if attempt == max || !statusMatches(buf.status, statuses) {
				buf.replay(w)
				return
			}
		}
	})
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
	for _, c := range codes {
		if c == status {
			return true
		}
	}
	return false
}
