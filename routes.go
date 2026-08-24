package statute

import "net/http"

// Route is a surface route declaration. Construct via Match.
type Route struct {
	pattern        string
	host           string
	clientIPs      []string
	clientIPsSet   bool
	upstream       string
	staticDir      string
	redirectTo     string
	redirectStatus int
	handler        http.Handler
	handlerSet     bool
	middleware     []Middleware
}

// Match begins a route declaration matching the given path pattern.
// Patterns support a trailing /* wildcard.
func Match(pattern string) *Route {
	return &Route{pattern: pattern}
}

// Host scopes this route to the given Host header value. Empty means any host.
func (r *Route) Host(host string) *Route {
	r.host = host
	return r
}

// ClientIPs scopes this route to clients inside the given CIDR ranges, as a
// matcher: a request from outside falls through to the next route instead
// of being rejected, which is what AllowIPs middleware would do. That
// enables conditional policies — a trusted-network route first, an
// authenticated fallback beneath it:
//
//	statute.Match("/*").Host("admin.example.com").ClientIPs("10.0.0.0/8").ProxyTo("admin"),
//	statute.Match("/*").Host("admin.example.com").ProxyTo("admin").With(statute.BasicAuth(...)),
//
// The client IP is the same verified resolution rate limiting and the IP
// lists use: the listener's TrustedProxy policy decides whether forwarded
// headers count. A client whose address cannot be parsed never matches a
// constrained route.
func (r *Route) ClientIPs(cidrs ...string) *Route {
	r.clientIPs = append(r.clientIPs, cidrs...)
	r.clientIPsSet = true
	return r
}

// ProxyTo proxies matching requests to the named upstream pool.
func (r *Route) ProxyTo(upstream string) *Route {
	r.upstream = upstream
	return r
}

// Serve serves matching requests as static files from the given directory.
func (r *Route) Serve(dir string) *Route {
	r.staticDir = dir
	return r
}

// RedirectTo answers matching requests with an HTTP redirect instead of
// proxying or serving files. The status must be one of 301, 302, 303, 307,
// or 308. The target may be fixed, or preserve parts of the request through
// placeholders substituted at request time:
//
//	{request_uri}  the request path and query, as sent ("/a/b?x=1")
//	{path}         the request path only
//	{query}        the raw query string, without the "?"
//	{host}         the request Host, port stripped
//
//	statute.Match("/*").Host("old.example.com").RedirectTo(
//		"https://new.example.com{request_uri}", http.StatusPermanentRedirect)
//
// This is the route-level counterpart of the listener-level
// Listener.RedirectTo, which redirects a whole listener to another scheme.
func (r *Route) RedirectTo(target string, status int) *Route {
	r.redirectTo = target
	r.redirectStatus = status
	return r
}

// Handle serves matching requests with an in-process http.Handler instead of
// proxying, serving files, or redirecting. It is the fourth mutually
// exclusive route action beside ProxyTo, Serve, and RedirectTo:
//
//	statute.Match("/healthz").Host("foo.example.com").Handle(http.HandlerFunc(
//		func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
//
// The handler composes with route middleware like any other action, and it
// receives the request path unstripped — the prefix stripping a wildcard
// Serve route performs is Serve-specific. Under a Retry middleware the
// handler may be re-entered once per attempt; that is intended, and Retry
// already confines itself to idempotent methods. Requests in the handler
// drain through normal graceful shutdown like proxied ones. The handler is
// invoked concurrently and must be safe for concurrent use.
func (r *Route) Handle(h http.Handler) *Route {
	r.handler = h
	r.handlerSet = true
	return r
}

// With attaches middleware to the route. Middleware runs in declaration order
// before the upstream proxy or static file handler.
func (r *Route) With(mws ...Middleware) *Route {
	r.middleware = append(r.middleware, mws...)
	return r
}
