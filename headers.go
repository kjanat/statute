package statute

import (
	"context"
	"io"
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

// SetRequestHeader replaces the named header on the request before the rest of
// the route runs, dropping whatever the client sent. Names are canonicalised
// when the configuration resolves, so "x-real-ip" and "X-Real-IP" name the
// same header.
//
// Four names are rejected at resolve time because Go carries them outside the
// header map, where a mutation here would be a silent no-op: Host (the request
// authority), Content-Length and Transfer-Encoding (the body framing), and
// Trailer (the trailer names). Hop-by-hop headers — Connection, Upgrade, TE,
// and the rest — can be set, but the reverse proxy strips them from the
// outbound request, as RFC 9110 requires.
func SetRequestHeader(name, value string) *headerMW {
	return &headerMW{op: resolved.MWSetRequestHeader, name: name, value: value}
}

// AddRequestHeader appends a value to the named request header, keeping the
// values already present.
func AddRequestHeader(name, value string) *headerMW {
	return &headerMW{op: resolved.MWAddRequestHeader, name: name, value: value}
}

// RemoveRequestHeader drops every value of the named request header before the
// rest of the route runs.
func RemoveRequestHeader(name string) *headerMW {
	return &headerMW{op: resolved.MWRemoveRequestHeader, name: name}
}

// SetResponseHeader replaces the named header on the way out, overriding
// whatever the upstream or file handler produced.
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

// isRequestHeaderOp reports whether op mutates the request.
func isRequestHeaderOp(op resolved.MiddlewareType) bool {
	switch op {
	case resolved.MWSetRequestHeader, resolved.MWAddRequestHeader, resolved.MWRemoveRequestHeader:
		return true
	default:
		return false
	}
}

// isResponseHeaderOp reports whether op mutates the response.
func isResponseHeaderOp(op resolved.MiddlewareType) bool {
	switch op {
	case resolved.MWSetResponseHeader, resolved.MWAddResponseHeader, resolved.MWRemoveResponseHeader:
		return true
	default:
		return false
	}
}

// applyHeaderOp performs one mutation on a header map. Names arrive canonical
// from resolve; Set, Add, and Del canonicalise again anyway.
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

// headerOp is one resolved mutation, compiled out of the middleware list when
// the route is built.
type headerOp struct {
	op    resolved.MiddlewareType
	name  string
	value string
}

// proxyForwardedHeaders are the fields httputil.ProxyRequest.SetXForwarded
// rewrites from the real connection. A route that declares its own value for
// one of them has to be reapplied afterwards, or the proxy's default silently
// wins — see forwardedOpsFromContext.
var proxyForwardedHeaders = map[string]bool{
	"X-Forwarded-For":   true,
	"X-Forwarded-Host":  true,
	"X-Forwarded-Proto": true,
}

// withHeaderMiddleware hoists a route's header operations out of the
// middleware chain and applies them at the route's edges: request mutations
// before anything else runs, response mutations when the header commits.
//
// Hoisting is what makes them exactly-once. Left in the chain, a Retry sitting
// outside them would re-enter the same handlers once per attempt, and an Add
// would stack a value per attempt. This handler runs once per request no
// matter how many attempts happen underneath it. It also gives the response
// wrapper a position outside any response buffering (retry, cache, ETag), so
// the mutations land on the response that is actually committed.
//
// Ordering among the operations themselves is the declared one; they do not
// interleave with the other middleware on the route.
func withHeaderMiddleware(mws []resolved.Middleware, next http.Handler) http.Handler {
	var requestOps, responseOps, forwardedOps []headerOp
	for _, m := range mws {
		op := headerOp{op: m.Type, name: m.HeaderName, value: m.HeaderValue}
		switch {
		case isRequestHeaderOp(m.Type):
			requestOps = append(requestOps, op)
			if proxyForwardedHeaders[op.name] {
				forwardedOps = append(forwardedOps, op)
			}
		case isResponseHeaderOp(m.Type):
			responseOps = append(responseOps, op)
		}
	}
	if len(requestOps) == 0 && len(responseOps) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, op := range requestOps {
			applyHeaderOp(r.Header, op.op, op.name, op.value)
		}
		if len(forwardedOps) > 0 {
			r = r.WithContext(context.WithValue(r.Context(), forwardedOpsKey{}, forwardedOps))
		}
		if len(responseOps) > 0 {
			w = &headerResponseWriter{ResponseWriter: w, ops: responseOps}
		}
		next.ServeHTTP(w, r)
	})
}

type forwardedOpsKey struct{}

// forwardedOpsFromContext returns the route's X-Forwarded-* operations, or nil
// when it declared none. The reverse proxy calls this after SetXForwarded to
// reapply what the route asked for: SetXForwarded derives those three fields
// from the real connection — which is what keeps a client from spoofing them —
// and in doing so overwrites the values a route configured on purpose.
func forwardedOpsFromContext(ctx context.Context) []headerOp {
	ops, _ := ctx.Value(forwardedOpsKey{}).([]headerOp)
	return ops
}

// headerResponseWriter applies a route's response-header operations when the
// response header is committed — by an explicit WriteHeader, the implicit one
// from a first Write, or a Flush on a streaming response.
//
// A protocol upgrade is the one response it does not touch: the reverse proxy
// hijacks the connection and writes the 101 handshake to it directly, from the
// upstream's response rather than through this writer. Hijacking still works
// (see Unwrap); the handshake simply is not a response this can rewrite.
type headerResponseWriter struct {
	http.ResponseWriter
	ops     []headerOp
	applied bool
}

// WriteHeader applies the operations, then writes the status. A 1xx is
// informational: net/http keeps the response open and the reverse proxy clears
// the header map right after, so those pass through untouched and the
// operations wait for the final status.
func (w *headerResponseWriter) WriteHeader(code int) {
	if code >= 200 {
		w.applyOps()
	}
	w.ResponseWriter.WriteHeader(code)
}

// Write applies the operations before the implicit 200 that an un-preceded
// body write commits.
func (w *headerResponseWriter) Write(b []byte) (int, error) {
	w.applyOps()
	return w.ResponseWriter.Write(b)
}

// ReadFrom applies the operations — a body copy commits the header just like
// Write — and hands the copy to the underlying writer, so io.Copy keeps the
// sendfile path net/http offers for file responses.
func (w *headerResponseWriter) ReadFrom(r io.Reader) (int64, error) {
	w.applyOps()
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(w.ResponseWriter, r)
}

// Flush applies the operations — a flush commits the header — and propagates
// the flush when the underlying writer supports it.
func (w *headerResponseWriter) Flush() {
	w.applyOps()
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying writer to http.ResponseController, so flushing
// and connection hijacking (WebSocket and other protocol upgrades) keep
// working through this wrapper.
func (w *headerResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// applyOps runs the operations once, in declaration order.
func (w *headerResponseWriter) applyOps() {
	if w.applied {
		return
	}
	w.applied = true
	h := w.Header()
	for _, op := range w.ops {
		applyHeaderOp(h, op.op, op.name, op.value)
	}
}
