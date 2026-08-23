package statute

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"statute.kjanat.dev/resolved"
)

// TestResolvePathRewrite — every constructor resolves to its own
// discriminator, with the normalised form the JSON export publishes.
func TestResolvePathRewrite(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name            string
		mw              Middleware
		wantType        resolved.MiddlewareType
		wantPrefix      string
		wantPattern     string
		wantReplacement string
		wantQuery       string
		wantQuerySet    bool
	}{
		{
			name:       "strip prefix",
			mw:         StripPrefix("/api"),
			wantType:   resolved.MWStripPrefix,
			wantPrefix: "/api",
		},
		{
			name:       "strip prefix trims a trailing slash",
			mw:         StripPrefix("/api/"),
			wantType:   resolved.MWStripPrefix,
			wantPrefix: "/api",
		},
		{
			name:       "strip prefix trims every trailing slash",
			mw:         StripPrefix("/api///"),
			wantType:   resolved.MWStripPrefix,
			wantPrefix: "/api",
		},
		{
			name:       "add prefix",
			mw:         AddPrefix("/v2/"),
			wantType:   resolved.MWAddPrefix,
			wantPrefix: "/v2",
		},
		{
			name:            "replace path without a query",
			mw:              ReplacePath("/x"),
			wantType:        resolved.MWReplacePath,
			wantReplacement: "/x",
		},
		{
			name:            "replace path with a query",
			mw:              ReplacePath("/x?a=1"),
			wantType:        resolved.MWReplacePath,
			wantReplacement: "/x",
			wantQuery:       "a=1",
			wantQuerySet:    true,
		},
		{
			name:            "replace path clearing the query",
			mw:              ReplacePath("/x?"),
			wantType:        resolved.MWReplacePath,
			wantReplacement: "/x",
			wantQuerySet:    true,
		},
		{
			name:            "replace path keeps the escaped form",
			mw:              ReplacePath("/a%2Fb"),
			wantType:        resolved.MWReplacePath,
			wantReplacement: "/a%2Fb",
		},
		{
			name:            "rewrite path",
			mw:              RewritePath(`^/api/v(\d+)/(.*)$`, "/v$1/$2"),
			wantType:        resolved.MWRewritePath,
			wantPattern:     `^/api/v(\d+)/(.*)$`,
			wantReplacement: "/v$1/$2",
		},
		{
			name:        "rewrite path allows an empty replacement",
			mw:          RewritePath("^/api", ""),
			wantType:    resolved.MWRewritePath,
			wantPattern: "^/api",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := mustResolveMW(t, c.mw)
			if got.Type != c.wantType {
				t.Errorf("type: got %v, want %v", got.Type, c.wantType)
			}
			if got.PathPrefix != c.wantPrefix {
				t.Errorf("PathPrefix: got %q, want %q", got.PathPrefix, c.wantPrefix)
			}
			if got.PathPattern != c.wantPattern {
				t.Errorf("PathPattern: got %q, want %q", got.PathPattern, c.wantPattern)
			}
			if got.PathReplacement != c.wantReplacement {
				t.Errorf("PathReplacement: got %q, want %q", got.PathReplacement, c.wantReplacement)
			}
			if got.PathQuery != c.wantQuery {
				t.Errorf("PathQuery: got %q, want %q", got.PathQuery, c.wantQuery)
			}
			if got.PathQuerySet != c.wantQuerySet {
				t.Errorf("PathQuerySet: got %v, want %v", got.PathQuerySet, c.wantQuerySet)
			}
		})
	}
}

// TestResolvePathRewriteErrors — a prefix or target that could not work is a
// startup error naming the constructor it came from, not a request-time
// surprise.
func TestResolvePathRewriteErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mw      Middleware
		wantErr string
	}{
		{"empty strip prefix", StripPrefix(""), `strip_prefix: prefix "" must start with "/"`},
		{"root strip prefix", StripPrefix("/"), `strip_prefix: prefix "/" must start with "/"`},
		{"slashes-only strip prefix", StripPrefix("///"), `strip_prefix: prefix "///" must start with "/"`},
		{"unrooted strip prefix", StripPrefix("api"), `strip_prefix: prefix "api" must start with "/"`},
		{"query in strip prefix", StripPrefix("/api?a=1"), `strip_prefix: prefix "/api?a=1" must not contain "?"`},
		{"fragment in strip prefix", StripPrefix("/api#top"), `strip_prefix: prefix "/api#top" must not contain "?"`},
		{"percent in strip prefix", StripPrefix("/api%2f"), `strip_prefix: prefix "/api%2f" must not contain "?", "#", or "%"`},
		{"protocol-relative strip prefix", StripPrefix("//evil.com"), `strip_prefix: prefix "//evil.com" must not start with "//"`},
		{"backslash strip prefix", StripPrefix("/\\evil.com"), `strip_prefix: prefix "/\\evil.com" must not start with "//" or "/\\"`},
		{"empty add prefix", AddPrefix(""), `add_prefix: prefix "" must start with "/"`},
		{"root add prefix", AddPrefix("/"), `add_prefix: prefix "/" must start with "/"`},
		{"unrooted add prefix", AddPrefix("v2"), `add_prefix: prefix "v2" must start with "/"`},
		{"query in add prefix", AddPrefix("/v2?a=1"), `add_prefix: prefix "/v2?a=1" must not contain "?"`},
		{"fragment in add prefix", AddPrefix("/v2#top"), `add_prefix: prefix "/v2#top" must not contain "?"`},
		{"percent in add prefix", AddPrefix("/v2%2f"), `add_prefix: prefix "/v2%2f" must not contain "?", "#", or "%"`},
		{"unrooted replace path", ReplacePath("x"), `replace_path: target path "x" must start with "/"`},
		{"bad escape in replace path", ReplacePath("/%zz"), `replace_path: target path "/%zz"`},
		{"fragment in replace path", ReplacePath("/x#top"), `replace_path: target "/x#top" must not contain "#"`},
		{"protocol-relative replace path", ReplacePath("//evil.com"), `replace_path: target path "//evil.com" must not start with "//"`},
		{"space in replace query", ReplacePath("/x?a=1 b"), `replace_path: target query "a=1 b" must not contain spaces or control characters`},
		{"empty rewrite pattern", RewritePath("", "/x"), "rewrite_path: pattern must not be empty"},
		{"invalid rewrite pattern", RewritePath("[", "/x"), "rewrite_path: error parsing regexp"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := resolveMiddleware(c.mw)
			if err == nil {
				t.Fatalf("want error containing %q", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("got %q, want substring %q", err, c.wantErr)
			}
		})
	}
}

// observedURL is what the innermost handler saw, the four fields that decide
// what the reverse proxy puts on the wire.
type observedURL struct {
	path       string
	rawPath    string
	rawQuery   string
	requestURI string
}

// recordURL returns a handler that records the URL it is handed.
func recordURL(seen *observedURL) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = observedURL{
			path:       r.URL.Path,
			rawPath:    r.URL.RawPath,
			rawQuery:   r.URL.RawQuery,
			requestURI: r.URL.RequestURI(),
		}
		w.WriteHeader(http.StatusOK)
	})
}

// runPathRewrite drives withPathRewrite directly over hand-built resolved
// middleware and returns what the wrapped handler observed.
func runPathRewrite(t *testing.T, mws []resolved.Middleware, target string) observedURL {
	t.Helper()
	var seen observedURL
	h := withPathRewrite(mws, recordURL(&seen))
	runRequest(t, h, httptest.NewRequest("GET", target, nil))
	return seen
}

// TestPathRewriteHandler — the transform matrix, driven at the handler level
// so each operation's effect on Path, RawPath, and the query is visible.
func TestPathRewriteHandler(t *testing.T) {
	t.Parallel()
	strip := resolved.Middleware{Type: resolved.MWStripPrefix, PathPrefix: "/api"}
	add := resolved.Middleware{Type: resolved.MWAddPrefix, PathPrefix: "/v2"}

	cases := []struct {
		name   string
		mws    []resolved.Middleware
		target string
		want   observedURL
	}{
		{
			name:   "strip the whole path leaves the root",
			mws:    []resolved.Middleware{strip},
			target: "/api",
			want:   observedURL{path: "/", requestURI: "/"},
		},
		{
			name:   "strip keeps the remainder and the query",
			mws:    []resolved.Middleware{strip},
			target: "/api/users?q=1",
			want:   observedURL{path: "/users", rawQuery: "q=1", requestURI: "/users?q=1"},
		},
		{
			name:   "a path without the prefix passes through untouched",
			mws:    []resolved.Middleware{strip},
			target: "/apiary/users?q=1",
			want:   observedURL{path: "/apiary/users", rawQuery: "q=1", requestURI: "/apiary/users?q=1"},
		},
		{
			name:   "strip preserves an escaped separator after the boundary",
			mws:    []resolved.Middleware{strip},
			target: "/api/a%2Fb",
			want:   observedURL{path: "/a/b", rawPath: "/a%2Fb", requestURI: "/a%2Fb"},
		},
		{
			// An escaped slash further along, past a real boundary, is
			// carried through verbatim — it has nothing to do with the strip.
			name:   "strip preserves a later escaped separator",
			mws:    []resolved.Middleware{strip},
			target: "/api/users/a%2Fb",
			want:   observedURL{path: "/users/a/b", rawPath: "/users/a%2Fb", requestURI: "/users/a%2Fb"},
		},
		{
			// The boundary slash is escaped, but the router matched "/api/*"
			// on the decoded path, so the strip fires too: the boundary "%2F"
			// becomes the new root "/", and the later "%2F" stays escaped —
			// never decoded into a second segment.
			name:   "strip normalises an escaped boundary and keeps the remainder",
			mws:    []resolved.Middleware{strip},
			target: "/api%2Ffoo%2Fbar",
			want:   observedURL{path: "/foo/bar", rawPath: "/foo%2Fbar", requestURI: "/foo%2Fbar"},
		},
		{
			// Both a "%2F" boundary and a later "%2F": the boundary is rooted
			// and every later escaped slash survives.
			name:   "strip normalises the boundary with a later escaped slash",
			mws:    []resolved.Middleware{strip},
			target: "/api%2Fusers/a%2Fb",
			want:   observedURL{path: "/users/a/b", rawPath: "/users/a%2Fb", requestURI: "/users/a%2Fb"},
		},
		{
			// The prefix itself arrives percent-encoded ("/a%70i" == "/api").
			// The walk decodes it, so the strip still fires and the remainder
			// escaping is preserved.
			name:   "strip matches a percent-encoded prefix",
			mws:    []resolved.Middleware{strip},
			target: "/a%70i/foo%2Fbar",
			want:   observedURL{path: "/foo/bar", rawPath: "/foo%2Fbar", requestURI: "/foo%2Fbar"},
		},
		{
			// The open-redirect vector: normalising the escaped boundary "%2F"
			// to "/" leaves the remainder rooted ("/@evil.com/x"), so a
			// redirect route's {path} cannot be steered off-site through it.
			name:   "strip roots an escaped-boundary at-sign path",
			mws:    []resolved.Middleware{strip},
			target: "/api%2F@evil.com/x",
			want:   observedURL{path: "/@evil.com/x", requestURI: "/@evil.com/x"},
		},
		{
			name:   "add prepends the prefix",
			mws:    []resolved.Middleware{add},
			target: "/users",
			want:   observedURL{path: "/v2/users", requestURI: "/v2/users"},
		},
		{
			name:   "add keeps the escaping of what it prepends to",
			mws:    []resolved.Middleware{add},
			target: "/a%2Fb?q=1",
			want:   observedURL{path: "/v2/a/b", rawPath: "/v2/a%2Fb", rawQuery: "q=1", requestURI: "/v2/a%2Fb?q=1"},
		},
		{
			name:   "replace preserves the query by default",
			mws:    []resolved.Middleware{{Type: resolved.MWReplacePath, PathReplacement: "/healthz"}},
			target: "/anything?q=1",
			want:   observedURL{path: "/healthz", rawQuery: "q=1", requestURI: "/healthz?q=1"},
		},
		{
			name: "replace overrides the query",
			mws: []resolved.Middleware{{
				Type: resolved.MWReplacePath, PathReplacement: "/healthz",
				PathQuery: "x=2", PathQuerySet: true,
			}},
			target: "/anything?q=1",
			want:   observedURL{path: "/healthz", rawQuery: "x=2", requestURI: "/healthz?x=2"},
		},
		{
			name: "replace clears the query",
			mws: []resolved.Middleware{{
				Type: resolved.MWReplacePath, PathReplacement: "/healthz", PathQuerySet: true,
			}},
			target: "/anything?q=1",
			want:   observedURL{path: "/healthz", requestURI: "/healthz"},
		},
		{
			name:   "replace keeps the escaped form it was given",
			mws:    []resolved.Middleware{{Type: resolved.MWReplacePath, PathReplacement: "/a%2Fb"}},
			target: "/anything",
			want:   observedURL{path: "/a/b", rawPath: "/a%2Fb", requestURI: "/a%2Fb"},
		},
		{
			name: "rewrite expands captures",
			mws: []resolved.Middleware{{
				Type: resolved.MWRewritePath, PathPattern: `^/api/v(\d+)/(.*)$`, PathReplacement: "/v$1/$2",
			}},
			target: "/api/v1/users?q=1",
			want:   observedURL{path: "/v1/users", rawQuery: "q=1", requestURI: "/v1/users?q=1"},
		},
		{
			name: "rewrite that drops the leading slash gets it back",
			mws: []resolved.Middleware{{
				Type: resolved.MWRewritePath, PathPattern: "^/api/", PathReplacement: "",
			}},
			target: "/api/users",
			want:   observedURL{path: "/users", requestURI: "/users"},
		},
		{
			name: "rewrite that empties the path yields the root",
			mws: []resolved.Middleware{{
				Type: resolved.MWRewritePath, PathPattern: "^/drop$", PathReplacement: "",
			}},
			target: "/drop",
			want:   observedURL{path: "/", requestURI: "/"},
		},
		{
			// Global, not first-match-only: both "/v1" segments rewrite.
			name: "rewrite replaces every match",
			mws: []resolved.Middleware{{
				Type: resolved.MWRewritePath, PathPattern: "/v1", PathReplacement: "/v2",
			}},
			target: "/v1/a/v1/b",
			want:   observedURL{path: "/v2/a/v2/b", requestURI: "/v2/a/v2/b"},
		},
		{
			// The pattern matches the DECODED path ("/api/a/b"), not the
			// escaped form ("/api/a%2Fb"): matching proves it operates on
			// the decoded path, and the result is canonically re-escaped.
			name: "rewrite matches the decoded path",
			mws: []resolved.Middleware{{
				Type: resolved.MWRewritePath, PathPattern: "^/api/a/b$", PathReplacement: "/x",
			}},
			target: "/api/a%2Fb",
			want:   observedURL{path: "/x", requestURI: "/x"},
		},
		{
			// A pattern that does not match leaves the URL — escaping and
			// all — exactly as it arrived; RawPath is not clobbered.
			name: "rewrite that does not match preserves the escaping",
			mws: []resolved.Middleware{{
				Type: resolved.MWRewritePath, PathPattern: "^/nomatch/", PathReplacement: "/x/",
			}},
			target: "/api/a%2Fb?q=1",
			want:   observedURL{path: "/api/a/b", rawPath: "/api/a%2Fb", rawQuery: "q=1", requestURI: "/api/a%2Fb?q=1"},
		},
		{
			// A hand-built empty target normalises to the root rather than
			// handing the inner handler an empty Path; the query survives.
			name:   "an empty replacement path normalises to the root",
			mws:    []resolved.Middleware{{Type: resolved.MWReplacePath, PathReplacement: ""}},
			target: "/anything?q=1",
			want:   observedURL{path: "/", rawQuery: "q=1", requestURI: "/?q=1"},
		},
		{
			// A bare "?" on the inbound request sets url.ForceQuery; clearing
			// the query has to clear that too, or RequestURI keeps the "?".
			name: "replace clears a forced empty query",
			mws: []resolved.Middleware{{
				Type: resolved.MWReplacePath, PathReplacement: "/healthz", PathQuerySet: true,
			}},
			target: "/anything?",
			want:   observedURL{path: "/healthz", requestURI: "/healthz"},
		},
		{
			name:   "operations apply in declaration order",
			mws:    []resolved.Middleware{strip, add},
			target: "/api/users",
			want:   observedURL{path: "/v2/users", requestURI: "/v2/users"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := runPathRewrite(t, c.mws, c.target)
			if got != c.want {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestStripPathPrefixInconsistentRawPath — a RawPath that real HTTP parsing
// cannot produce (out of step with Path) makes the raw walk fail; the strip
// still updates Path and drops RawPath so EscapedPath re-derives it from the
// decoded path rather than emitting something malformed.
func TestStripPathPrefixInconsistentRawPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		path    string
		rawPath string
		want    string // EscapedPath after stripping "/api"
	}{
		{"raw shorter than the prefix", "/api/x", "/ap", "/x"},
		{"raw diverges at the boundary", "/api/x", "/apiXy", "/x"},
		{"invalid escape inside the prefix span", "/api/x", "/a%zzi/x", "/x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			u := &url.URL{Path: c.path, RawPath: c.rawPath}
			stripPathPrefix(u, "/api")
			if got := u.EscapedPath(); got != c.want {
				t.Errorf("EscapedPath: got %q, want %q (path %q, rawPath %q)", got, c.want, u.Path, u.RawPath)
			}
		})
	}
}

// TestPathRewriteNoOps — a middleware list with no path rewrite in it returns
// the handler unchanged, and an uncompilable pattern is skipped rather than
// fatal, the way mustParsePrefixes skips an unparseable CIDR.
func TestPathRewriteNoOps(t *testing.T) {
	t.Parallel()
	base := http.NewServeMux()
	if got := withPathRewrite([]resolved.Middleware{{Type: resolved.MWTimeout}}, base); got != http.Handler(base) {
		t.Error("a chain with no path rewrite should be handed back unwrapped")
	}
	// A hand-built resolved config can carry a rewrite Resolve would have
	// rejected — an uncompilable pattern or a ReplacePath target with a bad
	// %-escape. Each is skipped rather than fatal, the way mustParsePrefixes
	// skips an unparseable CIDR, so the path arrives untouched.
	got := runPathRewrite(t, []resolved.Middleware{
		{Type: resolved.MWTimeout},
		{Type: resolved.MWRewritePath, PathPattern: "[", PathReplacement: "/x"},
		{Type: resolved.MWReplacePath, PathReplacement: "/%zz"},
	}, "/api/users?q=1")
	want := observedURL{path: "/api/users", rawQuery: "q=1", requestURI: "/api/users?q=1"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestPathRewriteDoesNotMutateOriginal — the handler clones the request and
// its URL, so whatever still holds the inbound request (the access log, an
// outer middleware) keeps seeing the path the client sent.
func TestPathRewriteDoesNotMutateOriginal(t *testing.T) {
	t.Parallel()
	var seen observedURL
	h := withPathRewrite([]resolved.Middleware{
		{Type: resolved.MWStripPrefix, PathPrefix: "/api"},
		{Type: resolved.MWAddPrefix, PathPrefix: "/v2"},
	}, recordURL(&seen))

	req := httptest.NewRequest("GET", "/api/users?q=1", nil)
	original := *req.URL
	runRequest(t, h, req)

	if seen.path != "/v2/users" {
		t.Fatalf("inner handler saw %q, want %q", seen.path, "/v2/users")
	}
	if *req.URL != original {
		t.Errorf("original URL mutated: got %+v, want %+v", *req.URL, original)
	}
	if req.URL.Path != "/api/users" {
		t.Errorf("original path: got %q, want %q", req.URL.Path, "/api/users")
	}
}

// TestPathRewriteHoistedAboveRetry pins that the rewrite lands outside the
// retry loop. The rewrite is declared *after* the retry, so if it were built
// into the chain it would sit inside retryHandler; the middlewareBuilders
// assertion below is what proves it is not, since a clone-based rewrite would
// otherwise still read "/v2/..." on both attempts and hide the difference.
// Both attempts observing the same rewritten path is the behaviour we keep.
func TestPathRewriteHoistedAboveRetry(t *testing.T) {
	t.Parallel()

	var paths []string
	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if len(paths) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	h := chain(t, base, Retry(2, OnStatus(http.StatusBadGateway)), AddPrefix("/v2"))
	rec := runRequest(t, h, httptest.NewRequest("GET", "/users", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	want := []string{"/v2/users", "/v2/users"}
	if len(paths) != len(want) || paths[0] != want[0] || paths[1] != want[1] {
		t.Fatalf("observed paths: got %v, want %v", paths, want)
	}

	// The hoist is what guarantees that: the path-rewrite discriminators are
	// deliberately absent from the in-chain builder table, exactly like the
	// header operations.
	for _, typ := range []resolved.MiddlewareType{
		resolved.MWStripPrefix, resolved.MWAddPrefix,
		resolved.MWReplacePath, resolved.MWRewritePath,
	} {
		if _, ok := middlewareBuilders[typ]; ok {
			t.Errorf("middleware type %v is in middlewareBuilders; it must stay hoisted", typ)
		}
	}
}

// TestEndToEndStripPrefix — the issue's example, end to end: the public
// "/api" prefix never reaches the upstream, and the query does.
func TestEndToEndStripPrefix(t *testing.T) {
	t.Parallel()
	uris := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uris <- r.RequestURI
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	cfg := Config{
		Listeners: Listeners{HTTP(":0")},
		Upstreams: Upstreams{
			"foo": Pool{Backends: []Backend{{Address: strings.TrimPrefix(upstream.URL, "http://")}}},
		},
		Routes: Routes{
			Match("/api/*").Host("foo.example.com").ProxyTo("foo").With(StripPrefix("/api")),
		},
	}
	srv, err := newServer(mustResolve(t, cfg))
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	t.Cleanup(func() {
		for _, ph := range srv.pools {
			ph.shutdown()
		}
	})

	req := httptest.NewRequest("GET", "http://foo.example.com/api/users?q=1", nil)
	rec := runRequest(t, srv.buildRouter(), req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := <-uris; got != "/users?q=1" {
		t.Errorf("upstream saw %q, want %q", got, "/users?q=1")
	}
}

// TestEndToEndStripPrefixRedirectStaysRooted — a StripPrefix on a redirect
// route must not let an escaped slash on the prefix boundary produce a
// Location whose authority the client controls. "/api%2F@evil.com/x" strips
// to a rooted "/@evil.com/x", so {path} builds a same-origin Location; the
// pre-fix code left "%2F@evil.com/x", which parses to host evil.com.
func TestEndToEndStripPrefixRedirectStaysRooted(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Listeners: Listeners{HTTP(":0")},
		Routes: Routes{
			Match("/api/*").
				RedirectTo("https://new.example.com{path}", http.StatusFound).
				With(StripPrefix("/api")),
		},
	}
	srv, err := newServer(mustResolve(t, cfg))
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}

	// StripPrefix normalises the escaped boundary "%2F" to "/", so {path}
	// resolves to a rooted "/@evil.com/x" and the authority stays glued to
	// new.example.com instead of introducing evil.com.
	req := httptest.NewRequest("GET", "/api%2F@evil.com/x", nil)
	rec := runRequest(t, srv.buildRouter(), req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if loc != "https://new.example.com/@evil.com/x" {
		t.Fatalf("Location: got %q, want %q", loc, "https://new.example.com/@evil.com/x")
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("Location %q does not parse: %v", loc, err)
	}
	if u.Host != "new.example.com" {
		t.Errorf("redirect authority: got %q, want new.example.com (open redirect)", u.Host)
	}
}

// TestExport_CarriesPathRewrites — the resolved JSON export publishes the
// normalised transform, which is what makes a deployment diffable.
func TestExport_CarriesPathRewrites(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Listeners: Listeners{HTTP(":8080")},
		Upstreams: Upstreams{"api": Pool{Backends: []Backend{{Address: "127.0.0.1:1"}}}},
		Routes: Routes{
			Match("/api/*").ProxyTo("api").With(
				StripPrefix("/api/"),
				AddPrefix("/v2///"),
				ReplacePath("/healthz?deep=1"),
				RewritePath(`^/v2/(.*)$`, "/$1"),
			),
		},
	}
	var buf strings.Builder
	if err := Export(cfg, &buf); err != nil {
		t.Fatalf("Export: %v", err)
	}
	var out struct {
		Routes []struct {
			Middleware []exportedPathMW
		}
	}
	if err := json.Unmarshal([]byte(buf.String()), &out); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	if len(out.Routes) != 1 || len(out.Routes[0].Middleware) != 4 {
		t.Fatalf("export shape: %s", buf.String())
	}
	mws := out.Routes[0].Middleware

	for i, w := range wantPathMiddleware() {
		assertPathMiddleware(t, i, mws[i], w)
	}
}

// exportedPathMW is the resolved-export shape of one path rewrite, decoded
// back out of the JSON.
type exportedPathMW struct {
	Type            int
	PathPrefix      string
	PathPattern     string
	PathReplacement string
	PathQuery       string
	PathQuerySet    bool
}

// wantPathMiddleware is the normalised export TestExport_CarriesPathRewrites
// expects for its four declarations.
func wantPathMiddleware() []exportedPathMW {
	return []exportedPathMW{
		{Type: int(resolved.MWStripPrefix), PathPrefix: "/api"},
		{Type: int(resolved.MWAddPrefix), PathPrefix: "/v2"},
		{Type: int(resolved.MWReplacePath), PathReplacement: "/healthz", PathQuery: "deep=1", PathQuerySet: true},
		{Type: int(resolved.MWRewritePath), PathPattern: `^/v2/(.*)$`, PathReplacement: "/$1"},
	}
}

// assertPathMiddleware compares one exported middleware against what resolve
// should have normalised it to.
func assertPathMiddleware(t *testing.T, i int, got, want exportedPathMW) {
	t.Helper()
	if got != want {
		t.Errorf("middleware[%d]: got %+v, want %+v", i, got, want)
	}
}
