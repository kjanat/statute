package statute

// Route is a surface route declaration. Construct via Match.
type Route struct {
	pattern    string
	host       string
	upstream   string
	staticDir  string
	middleware []Middleware
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

// With attaches middleware to the route. Middleware runs in declaration order
// before the upstream proxy or static file handler.
func (r *Route) With(mws ...Middleware) *Route {
	r.middleware = append(r.middleware, mws...)
	return r
}
