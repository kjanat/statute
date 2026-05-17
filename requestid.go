package statute

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"github.com/kjanat/statute/resolved"
)

type requestIDMW struct {
	header     string
	fromHeader string
}

func (*requestIDMW) statuteMiddleware() {}

// RequestID returns a middleware that ensures every request carries a stable
// identifier — useful for tracing through logs and propagating to upstream
// backends. The default response header is defaultRequestIDHeader; override with Header.
//
// If an inbound header (configured via From) is present, its value is used
// verbatim. Otherwise a new ID is generated from 16 bytes of crypto/rand,
// hex-encoded.
//
// When the access log is configured, the request ID surfaces as a
// "request_id" field on log lines.
func RequestID() *requestIDMW {
	return &requestIDMW{header: defaultRequestIDHeader}
}

// Header overrides the name of the response header that carries the ID.
func (r *requestIDMW) Header(name string) *requestIDMW {
	r.header = name
	return r
}

// From sets a name of an inbound request header to consult before generating
// a fresh ID. Useful when statute sits behind another proxy or service that
// already injects a trace ID (e.g. "X-Cloud-Trace-Context").
func (r *requestIDMW) From(name string) *requestIDMW {
	r.fromHeader = name
	return r
}

const defaultRequestIDHeader = "X-Request-Id"

type ridCtxKey struct{}

// requestIDFromContext returns the request ID set by requestIDHandler, or "" if none.
func requestIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ridCtxKey{}).(string)
	return v
}

// requestIDHandler attaches the configured request ID to the request context
// (so the access log middleware can pick it up), sets it on the outgoing
// request headers (so upstream backends see it), and emits it on the
// response. Generation is best-effort: on rand.Read failure, the request
// still proceeds without an ID rather than failing.
func requestIDHandler(m resolved.Middleware, next http.Handler) http.Handler {
	respHeader := m.RequestIDHeader
	if respHeader == "" {
		respHeader = defaultRequestIDHeader
	}
	from := m.RequestIDFromHeader
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := ""
		if from != "" {
			id = r.Header.Get(from)
		}
		if id == "" {
			id = newRequestID()
		}
		if id != "" {
			r.Header.Set(respHeader, id)
			w.Header().Set(respHeader, id)
			r = r.WithContext(context.WithValue(r.Context(), ridCtxKey{}, id))
		}
		next.ServeHTTP(w, r)
	})
}

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}
