package statute

import (
	"context"
	"net/http"

	"statute.kjanat.dev/resolved"
)

// headerMW is one header mutation. The six constructors below differ only in
// the operation they carry, so they share one type instead of repeating the
// same two fields six times.
type headerMW struct {
	op    resolved.MiddlewareType
	name  string
	value string
}

func (*headerMW) statuteMiddleware() {}

// SetRequestHeader replaces the named header on the request before it reaches
// the route's proxy or file handler, dropping whatever the client sent. Names
// are canonicalised when the configuration resolves, so "x-real-ip" and
// "X-Real-IP" name the same header.
//
// Host is rejected: Go carries the request authority in a dedicated field
// that the transport writes from, so mutating the header map would be
// silently ignored.
//
// Two other groups are not yours to set on a proxy route, because the reverse
// proxy rewrites them after route middleware has run: the X-Forwarded-For,
// X-Forwarded-Host, and X-Forwarded-Proto set, which the proxy derives from
// the real connection so a client cannot spoof them, and the hop-by-hop
// headers (Connection, Upgrade, …), which it strips.
func SetRequestHeader(name, value string) *headerMW {
	return &headerMW{op: resolved.MWSetRequestHeader, name: name, value: value}
}

// AddRequestHeader appends a value to the named request header, keeping the
// values already present.
func AddRequestHeader(name, value string) *headerMW {
	return &headerMW{op: resolved.MWAddRequestHeader, name: name, value: value}
}

// RemoveRequestHeader drops every value of the named request header before
// the request is forwarded.
func RemoveRequestHeader(name string) *headerMW {
	return &headerMW{op: resolved.MWRemoveRequestHeader, name: name}
}

// SetResponseHeader replaces the named header on the way out, overriding
// whatever the upstream or file handler produced. Response mutations apply
// when the response header is committed, so they win over the origin.
func SetResponseHeader(name, value string) *headerMW {
	return &headerMW{op: resolved.MWSetResponseHeader, name: name, value: value}
}

// AddResponseHeader appends a value to the named response header, keeping the
// values the upstream or file handler produced.
func AddResponseHeader(name, value string) *headerMW {
	return &headerMW{op: resolved.MWAddResponseHeader, name: name, value: value}
}

// RemoveResponseHeader drops every value of the named response header before
// the response is committed.
func RemoveResponseHeader(name string) *headerMW {
	return &headerMW{op: resolved.MWRemoveResponseHeader, name: name}
}

// headerMWLabel names the surface constructor an operation came from, for
// resolve-time error messages.
func headerMWLabel(op resolved.MiddlewareType) string {
	switch op {
	case resolved.MWSetRequestHeader:
		return "set_request_header"
	case resolved.MWAddRequestHeader:
		return "add_request_header"
	case resolved.MWRemoveRequestHeader:
		return "remove_request_header"
	case resolved.MWSetResponseHeader:
		return "set_response_header"
	case resolved.MWAddResponseHeader:
		return "add_response_header"
	case resolved.MWRemoveResponseHeader:
		return "remove_response_header"
	default:
		return enumUnknown
	}
}

// isRequestHeaderOp reports whether op mutates the request rather than the
// response.
func isRequestHeaderOp(op resolved.MiddlewareType) bool {
	switch op {
	case resolved.MWSetRequestHeader, resolved.MWAddRequestHeader, resolved.MWRemoveRequestHeader:
		return true
	default:
		return false
	}
}

// applyHeaderOp performs one mutation on a header map. Names arrive
// canonical from resolve; Set, Add, and Del canonicalise again anyway.
func applyHeaderOp(h http.Header, op resolved.MiddlewareType, name, value string) {
	switch op {
	case resolved.MWSetRequestHeader, resolved.MWSetResponseHeader:
		h.Set(name, value)
	case resolved.MWAddRequestHeader, resolved.MWAddResponseHeader:
		h.Add(name, value)
	case resolved.MWRemoveRequestHeader, resolved.MWRemoveResponseHeader:
		h.Del(name)
	default:
	}
}

// requestHeaderHandler mutates the inbound request header before passing the
// request on. Middleware runs outermost-first, so a route's request
// mutations apply in declaration order.
func requestHeaderHandler(m resolved.Middleware, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		applyHeaderOp(r.Header, m.Type, m.HeaderName, m.HeaderValue)
		next.ServeHTTP(w, r)
	})
}

// headerOp is one response-header mutation awaiting the response.
type headerOp struct {
	op    resolved.MiddlewareType
	name  string
	value string
}

// pendingHeaderOps collects a route's response-header mutations, in
// declaration order, for the single ResponseWriter wrapper that applies them.
//
// Response mutations cannot run on the way in — the upstream response has
// not been produced yet, and a proxy overwrites the header map wholesale
// when it arrives. Wrapping the writer once per middleware would instead
// apply them innermost-first, reversing the declared order, so the first
// response-header middleware on the route installs the wrapper and shares
// this list through the request context; the rest append to it.
type pendingHeaderOps struct{ ops []headerOp }

type pendingHeaderOpsKey struct{}

// responseHeaderHandler registers one response-header mutation for the
// request, installing the writer wrapper if it is the first on the route.
func responseHeaderHandler(m resolved.Middleware, next http.Handler) http.Handler {
	op := headerOp{op: m.Type, name: m.HeaderName, value: m.HeaderValue}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if pending, ok := r.Context().Value(pendingHeaderOpsKey{}).(*pendingHeaderOps); ok {
			pending.ops = append(pending.ops, op)
			next.ServeHTTP(w, r)
			return
		}
		pending := &pendingHeaderOps{ops: []headerOp{op}}
		ctx := context.WithValue(r.Context(), pendingHeaderOpsKey{}, pending)
		next.ServeHTTP(&headerResponseWriter{ResponseWriter: w, pending: pending}, r.WithContext(ctx))
	})
}

// headerResponseWriter applies the route's response-header mutations when the
// response header is committed — by an explicit WriteHeader, an implicit one
// from the first Write, or a Flush on a streaming response.
type headerResponseWriter struct {
	http.ResponseWriter
	pending *pendingHeaderOps
	applied bool
}

// WriteHeader applies the pending mutations, then writes the status.
func (w *headerResponseWriter) WriteHeader(code int) {
	w.applyOps()
	w.ResponseWriter.WriteHeader(code)
}

// Write applies the pending mutations before the implicit 200 that an
// un-preceded body write commits.
func (w *headerResponseWriter) Write(b []byte) (int, error) {
	w.applyOps()
	return w.ResponseWriter.Write(b)
}

// Flush applies the pending mutations — a flush commits the header — and
// propagates the flush when the underlying writer supports it.
func (w *headerResponseWriter) Flush() {
	w.applyOps()
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying writer to http.ResponseController, so
// flushing and connection hijacking (WebSocket and other protocol upgrades)
// keep working through this wrapper.
func (w *headerResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// applyOps runs the collected mutations once, in declaration order.
func (w *headerResponseWriter) applyOps() {
	if w.applied {
		return
	}
	w.applied = true
	h := w.Header()
	for _, op := range w.pending.ops {
		applyHeaderOp(h, op.op, op.name, op.value)
	}
}
