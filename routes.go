package statute

import "net/http"

// Route is a surface route declaration. Construct via Match.
type Route struct {
	priority             int
	listeners            []string
	pathMatch            string
	pattern              string
	host                 string
	hostSet              bool
	hosts                []string
	hostsSet             bool
	clientIPs            []string
	clientIPsSet         bool
	peerIPs              []string
	peerIPsSet           bool
	upstream             string
	discoveredUpstream   string
	staticDir            string
	redirectTo           string
	redirectStatus       int
	redirectNonGETStatus int
	handler              http.Handler
	handlerSet           bool
	middleware           []Middleware
}

// Match begins a route declaration matching the given path pattern.
// Patterns support a trailing /* wildcard.
func Match(pattern string) *Route {
	return &Route{pattern: pattern}
}

// MatchExact matches a literal decoded path, including a trailing /*.
func MatchExact(path string) *Route { return &Route{pattern: path, pathMatch: "exact"} }

// MatchPrefix matches a byte prefix, including prefixes inside a segment.
func MatchPrefix(prefix string) *Route { return &Route{pattern: prefix, pathMatch: "byte_prefix"} }

// Priority participates in selection when Config.PrioritizeRoutes is enabled.
func (r *Route) Priority(priority int) *Route { r.priority = priority; return r }

// OnListeners limits this route to the named listeners.
func (r *Route) OnListeners(names ...string) *Route {
	r.listeners = append(r.listeners, names...)
	return r
}

// Host scopes this route to the given Host header value. Empty means any host.
func (r *Route) Host(host string) *Route {
	r.host = host
	r.hostSet = true
	return r
}

// Hosts appends explicit host values to this declaration, copying the inputs.
// Resolve expands them in order into consecutive ordinary Host routes, each
// with the same action, client-IP matcher and middleware declarations.
//
// The final list must be nonempty, contain no empty entry or case-insensitive
// duplicate, and cannot be combined with Host. Values retain their spelling
// and the existing Host matching semantics; no DNS normalization is applied.
func (r *Route) Hosts(hosts ...string) *Route {
	r.hosts = append(r.hosts, hosts...)
	r.hostsSet = true
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

// PeerIPs matches the direct connection address, ignoring forwarded headers.
// A non-matching peer falls through to the next route.
func (r *Route) PeerIPs(cidrs ...string) *Route {
	r.peerIPs = append(r.peerIPs, cidrs...)
	r.peerIPsSet = true
	return r
}

// ProxyTo proxies matching requests to the named upstream pool.
func (r *Route) ProxyTo(upstream string) *Route {
	r.upstream = upstream
	return r
}

// ProxyToDiscovered selects a Docker pool from the current discovery generation.
// The route keeps its own middleware; backend health and transport remain shared.
func (r *Route) ProxyToDiscovered(name string) *Route { r.discoveredUpstream = name; return r }

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

// RedirectNonGET selects the redirect status for methods other than GET.
func (r *Route) RedirectNonGET(status int) *Route { r.redirectNonGETStatus = status; return r }

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

// WithOuter prepends middleware ahead of the route's existing chain.
func (r *Route) WithOuter(mws ...Middleware) *Route {
	r.middleware = append(append([]Middleware(nil), mws...), r.middleware...)
	return r
}

// With attaches middleware in declaration order before the route handler.
func (r *Route) With(mws ...Middleware) *Route {
	r.middleware = append(r.middleware, mws...)
	return r
}
