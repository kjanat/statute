package statute

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"statute.kjanat.dev/resolved"
)

// The four path-rewrite constructors below share one runtime: withPathRewrite
// hoists them out of the middleware chain and applies them once per request,
// in declaration order, at the route's edge. Their common contract, repeated
// on each constructor because that is where godoc readers land:
//
//   - Route matching happens first, so the route pattern and the access log
//     observe the original request path; the rewritten path is what the cache
//     key, the remaining middleware, and the upstream see.
//   - The rewrite is applied exactly once per request, no matter how many
//     upstream attempts happen beneath it, so a Retry declared anywhere on
//     the route cannot reapply it per attempt.
//   - The query string is carried through untouched. ReplacePath's explicit
//     "?query" suffix is the only construct that changes it.

type stripPrefixMW struct{ prefix string }

func (*stripPrefixMW) statuteMiddleware() {}

// StripPrefix removes prefix from the request path before the request is
// proxied or served, which is how a public path maps onto a backend that
// knows nothing about it:
//
//	statute.Match("/api/*").Host("foo.example.com").ProxyTo("foo").
//		With(statute.StripPrefix("/api"))
//
// A request for "/api/users?q=1" reaches the upstream as "/users?q=1". The
// prefix is a decoded literal, normalised when the configuration resolves:
// trailing slashes are trimmed, so "/api/" and "/api" are the same
// declaration, and a prefix that does not name at least one segment ("", "/",
// "///"), that carries a "?", "#", or "%", or that starts with a doubled slash
// ("//", "/\") is a startup error.
//
// Stripping the whole path leaves "/". A path the prefix does not cover is
// passed through untouched rather than rejected — the route pattern normally
// guarantees the prefix, and a route that admits other paths should see them
// unchanged rather than mangled.
//
// This is a transform for proxy routes. A static route already strips its own
// wildcard prefix before serving (Match("/assets/*").Serve(dir) looks up
// "/assets/x" as "x"), so adding StripPrefix("/assets") on top strips twice
// and the file lookup misses; use one or the other, not both.
//
// Like the header operations, path rewrites are hoisted out of the middleware
// chain and applied at the route's edge, before every other middleware, in
// declaration order among themselves — so the prefix is removed exactly once
// regardless of where the rewrite sits in the .With(...) list or how many
// times a Retry underneath re-attempts. They run after route matching, so the
// route pattern and the access log see the original path. The query string is
// preserved.
func StripPrefix(prefix string) *stripPrefixMW { return &stripPrefixMW{prefix: prefix} }

type addPrefixMW struct{ prefix string }

func (*addPrefixMW) statuteMiddleware() {}

// AddPrefix prepends prefix to the request path before the request is proxied
// or served, the inverse of StripPrefix: a backend mounted under "/v2" can be
// reached at the root of a route.
//
// The prefix is a decoded literal, normalised when the configuration resolves
// exactly as StripPrefix's is — trailing slashes trimmed, and an empty,
// slash-only, or "?"/"#"/"%"-carrying prefix is a startup error.
//
// Like the header operations, path rewrites are hoisted out of the middleware
// chain and applied at the route's edge, before every other middleware, in
// declaration order among themselves — so the prefix is prepended exactly
// once no matter where the rewrite sits in the .With(...) list or how many
// times a Retry underneath re-attempts. They run after route matching, so the
// route pattern and the access log see the original path. The query string is
// preserved.
func AddPrefix(prefix string) *addPrefixMW { return &addPrefixMW{prefix: prefix} }

type replacePathMW struct{ path string }

func (*replacePathMW) statuteMiddleware() {}

// ReplacePath discards the request path and substitutes a fixed one, whatever
// the client asked for:
//
//	statute.Match("/health/*").ProxyTo("api").With(statute.ReplacePath("/healthz"))
//
// The target must start with "/" and must be a valid escaped path — an
// invalid %-escape or a "#" is a startup error. An escaped target is sent in
// the escaped form given, so "%2F" stays a literal slash in a segment rather
// than becoming a separator; a target that is already canonical is sent as
// written, and anything else is re-escaped canonically.
//
// A "?" in the target explicitly replaces the query string, and is the one
// place in the path-rewrite set where the query changes. The replacement query
// goes onto the request target verbatim, so it too must be free of spaces and
// control bytes (escape a space as "%20") or resolving is a startup error:
//
//	ReplacePath("/healthz")        // path replaced, incoming query preserved
//	ReplacePath("/healthz?deep=1") // query replaced with "deep=1"
//	ReplacePath("/healthz?")       // query cleared
//
// Like the header operations, path rewrites are hoisted out of the middleware
// chain and applied once per request at the route's edge, in declaration order
// among themselves, so a Retry underneath cannot reapply them per attempt.
// They run after route matching, so the route pattern and the access log see
// the original path.
func ReplacePath(path string) *replacePathMW { return &replacePathMW{path: path} }

type rewritePathMW struct {
	pattern     string
	replacement string
}

func (*rewritePathMW) statuteMiddleware() {}

// RewritePath rewrites the request path through a regular expression, which is
// the primitive the other three cannot express: moving captured segments
// around.
//
//	statute.Match("/api/*").ProxyTo("api").
//		With(statute.RewritePath(`^/api/v(\d+)/(.*)$`, "/v$1/$2"))
//
// The pattern is RE2 (Go's regexp syntax), matched against the decoded path,
// and the replacement follows regexp.ReplaceAllString: "$1" and "${name}"
// expand to captures, and every match in the path is replaced, not just the
// first. An empty pattern or one that does not compile is a startup error; an
// empty replacement is allowed and deletes what it matches.
//
// Matching the decoded path means a "%2F" the client escaped inside a segment
// is seen by the pattern as a real separator, and the rewritten path is sent
// in its canonical escaping; a request the pattern does not match is left
// exactly as it arrived, escaping and all.
//
// The result is normalised to a rooted path: a rewrite that leaves the leading
// slash off gets one back, and a rewrite that empties the path yields "/".
//
// Like the header operations, path rewrites are hoisted out of the middleware
// chain and applied once per request at the route's edge, in declaration order
// among themselves, so a Retry underneath cannot reapply them per attempt.
// They run after route matching, so the route pattern and the access log see
// the original path. The query string is preserved; only ReplacePath's
// explicit "?query" form changes it.
func RewritePath(pattern, replacement string) *rewritePathMW {
	return &rewritePathMW{pattern: pattern, replacement: replacement}
}

// pathOp is one resolved path transform, compiled out of the middleware list
// when the route is built. Each closure captures exactly the fields its
// operation needs and applies them to the request URL in place.
type pathOp func(*url.URL)

// withPathRewrite hoists a route's path rewrites out of the middleware chain
// and applies them at the route's edge, before anything else on the route
// runs, in the order they were declared.
//
// Hoisting makes the transform independent of where the rewrites sit in the
// .With(...) list: whatever else the route carries, the rewrite lands first,
// so the cache key, the remaining middleware, and the upstream request the
// reverse proxy builds from r.URL all observe the same rewritten path, and a
// Retry beneath can never re-enter a half-rewritten one. The rewrite works on
// a clone of the request rather than mutating it in place — which is what
// keeps it exactly-once under a retry that re-serves the same request per
// attempt — so this sits inside withHeaderMiddleware, which owns the outer
// edge because its operations do mutate the request in place.
//
// The rewrite happens after the router has already chosen this route, so the
// route pattern is written against the path the client sent, and the access
// log — which observes the request on its way into the router — records that
// original path too. Everything downstream of this handler sees the rewritten
// path: the cache key, the remaining middleware, and the upstream request the
// reverse proxy builds from r.URL.
//
// A pattern that fails to compile here is skipped rather than fatal, following
// mustParsePrefixes: Resolve already compiled it, so only a hand-built
// resolved.Config can carry a broken one.
func withPathRewrite(mws []resolved.Middleware, next http.Handler) http.Handler {
	var ops []pathOp
	for _, m := range mws {
		if op, ok := compilePathOp(m); ok {
			ops = append(ops, op)
		}
	}
	if len(ops) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Shallow-clone the request and its URL, the http.StripPrefix
		// pattern: the caller's request is never mutated, so anything
		// holding it — the access log, an outer middleware — keeps seeing
		// the path the client sent.
		r2 := new(http.Request)
		*r2 = *r
		r2.URL = new(url.URL)
		*r2.URL = *r.URL
		for _, op := range ops {
			op(r2.URL)
		}
		if r2.URL.Path == "" {
			r2.URL.Path = "/"
			r2.URL.RawPath = ""
		}
		next.ServeHTTP(w, r2)
	})
}

// compilePathOp turns one resolved middleware into a path transform, reporting
// false for every middleware that is not a path rewrite and for a rewrite
// pattern that does not compile.
func compilePathOp(m resolved.Middleware) (pathOp, bool) {
	switch m.Type {
	case resolved.MWStripPrefix:
		prefix := m.PathPrefix
		return func(u *url.URL) { stripPathPrefix(u, prefix) }, true
	case resolved.MWAddPrefix:
		prefix := m.PathPrefix
		return func(u *url.URL) { addPathPrefix(u, prefix) }, true
	case resolved.MWReplacePath:
		replacement, query, querySet := m.PathReplacement, m.PathQuery, m.PathQuerySet
		return func(u *url.URL) { replaceURLPath(u, replacement, query, querySet) }, true
	case resolved.MWRewritePath:
		re, err := regexp.Compile(m.PathPattern)
		if err != nil {
			return nil, false
		}
		replacement := m.PathReplacement
		return func(u *url.URL) { rewriteURLPath(u, re, replacement) }, true
	default:
		return nil, false
	}
}

// stripPathPrefix removes prefix from u's path, leaving "/" when the prefix
// was the whole path and leaving the URL completely untouched when the path
// does not carry the prefix — the route pattern normally guarantees it, and a
// path that arrives without it passes through rather than 404s.
//
// RawPath is cut literally, the way http.StripPrefix does it, so a "%2F"
// after the prefix boundary survives the strip. The cut is kept only when
// what remains is still a rooted path: an escaped slash sitting on the prefix
// boundary ("/api%2Fusers") would otherwise leave RawPath without a leading
// "/", so EscapedPath() would emit a relative reference — a wrong upstream
// target, and on a redirect route a Location whose authority the client
// controls. When the remainder is not rooted, RawPath is dropped so the
// canonical escaping of the new Path takes over.
func stripPathPrefix(u *url.URL, prefix string) {
	switch {
	case u.Path == prefix:
		u.Path = "/"
	case strings.HasPrefix(u.Path, prefix+"/"):
		u.Path = u.Path[len(prefix):]
	default:
		return
	}
	if u.RawPath == "" {
		return
	}
	if rest, ok := strings.CutPrefix(u.RawPath, prefix); ok && strings.HasPrefix(rest, "/") {
		u.RawPath = rest
		return
	}
	u.RawPath = ""
}

// addPathPrefix prepends prefix to u's path, escaping it into RawPath as well
// when the request carried an escaped form worth preserving.
func addPathPrefix(u *url.URL, prefix string) {
	u.Path = prefix + u.Path
	if u.RawPath != "" {
		u.RawPath = (&url.URL{Path: prefix}).EscapedPath() + u.RawPath
	}
}

// replaceURLPath sets u's path to the configured target, keeping the escaped
// form the operator wrote, and replaces the query only when the target carried
// a "?". The unescape cannot fail for a resolver-produced configuration, which
// validated it; a hand-built one that fails skips the operation.
func replaceURLPath(u *url.URL, replacement, query string, querySet bool) {
	decoded, err := url.PathUnescape(replacement)
	if err != nil {
		return
	}
	u.Path = decoded
	if replacement != decoded {
		u.RawPath = replacement
	} else {
		u.RawPath = ""
	}
	if querySet {
		// ForceQuery rides along from the inbound URL through the clone; a
		// request for "/x?" carries it set with an empty RawQuery, which
		// keeps RequestURI() emitting the "?". Clearing it here is what lets
		// ReplacePath("/x?") actually drop the query.
		u.RawQuery = query
		u.ForceQuery = false
	}
}

// rewriteURLPath applies the regexp rewrite to u's decoded path and normalises
// the result to a rooted path. The match runs on the decoded path, so a "%2F"
// the client escaped inside a segment becomes a real separator to the pattern;
// when the rewrite changes the path, RawPath is dropped because the canonical
// escaping of the new path is the only encoding that can still be correct.
// A rewrite that leaves the path unchanged — a pattern that did not match, or
// a replacement equal to the input — leaves the URL untouched, so a request
// the rewrite does not apply to keeps the escaping the client sent.
func rewriteURLPath(u *url.URL, re *regexp.Regexp, replacement string) {
	out := re.ReplaceAllString(u.Path, replacement)
	if out == "" {
		out = "/"
	}
	if !strings.HasPrefix(out, "/") {
		out = "/" + out
	}
	if out == u.Path {
		return
	}
	u.Path = out
	u.RawPath = ""
}
