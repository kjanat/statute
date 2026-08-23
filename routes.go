package statute

// Route is a surface route declaration. Construct via Match.
type Route struct {
	pattern        string
	host           string
	upstream       string
	staticDir      string
	redirectTo     string
	redirectStatus int
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

// With attaches middleware to the route. Middleware runs in declaration order
// before the upstream proxy or static file handler.
func (r *Route) With(mws ...Middleware) *Route {
	r.middleware = append(r.middleware, mws...)
	return r
}
