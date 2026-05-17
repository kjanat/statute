package statute

import (
	"net/http"

	"statute.kjanat.dev/resolved"
)

type bodyLimitMW struct{ size string }

func (*bodyLimitMW) statuteMiddleware() {}

// BodyLimit returns a middleware that caps the request body at the given
// size. Sizes are strings like "1MB", "512KiB", or "10485760". Requests with
// a body larger than the limit receive a 413 Request Entity Too Large response;
// the upstream handler never sees them.
//
// The cap is enforced via http.MaxBytesReader on r.Body; calls to Read past
// the limit return an error, which the wrapped handler is expected to surface
// as 413. For upstream proxies the reverse-proxy transport handles this
// transparently.
func BodyLimit(size string) *bodyLimitMW {
	return &bodyLimitMW{size: size}
}

// bodyLimitHandler enforces the resolved byte limit.
func bodyLimitHandler(m resolved.Middleware, next http.Handler) http.Handler {
	limit := m.BodyLimitBytes
	if limit <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
}
