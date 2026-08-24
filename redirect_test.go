package statute

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// redirectConfig is a minimal config whose only route redirects.
func redirectConfig(target string, status int) Config {
	return Config{
		Listeners: Listeners{HTTP(":0")},
		Routes:    Routes{Match("/*").RedirectTo(target, status)},
	}
}

// TestResolveRedirect — every allowed status resolves, and the resolved
// schema names the action explicitly.
func TestResolveRedirect(t *testing.T) {
	t.Parallel()
	for _, status := range []int{301, 302, 303, 307, 308} {
		r := mustResolve(t, redirectConfig("https://new.example.com{request_uri}", status))
		rd := r.Routes[0].Redirect
		if rd == nil {
			t.Fatalf("status %d: resolved route has no Redirect", status)
		}
		if rd.Target != "https://new.example.com{request_uri}" || rd.Status != status {
			t.Errorf("status %d: got %+v", status, rd)
		}
		if r.Routes[0].Upstream != nil || r.Routes[0].StaticDir != "" {
			t.Errorf("status %d: redirect route carries another action: %+v", status, r.Routes[0])
		}
	}
}

// TestResolveRedirectErrors — bad declarations fail at resolve time, each
// with a message naming the problem.
func TestResolveRedirectErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{"empty target", redirectConfig("", 301), "target is empty"},
		{"status not a redirect", redirectConfig("/new", 304), "not a redirect status"},
		{"status out of class", redirectConfig("/new", 200), "not a redirect status"},
		{"status zero", redirectConfig("/new", 0), "not a redirect status"},
		{"unknown placeholder", redirectConfig("/new/{foo}", 301), `unknown placeholder "{foo}"`},
		{"unclosed placeholder", redirectConfig("/new/{path", 301), "unclosed placeholder"},
		{"header injection", redirectConfig("/new\r\nSet-Cookie: x", 301), "invalid character"},
		{
			"redirect plus proxy",
			Config{
				Listeners: Listeners{HTTP(":0")},
				Upstreams: Upstreams{"api": Pool{Backends: []Backend{{Address: "127.0.0.1:9001"}}}},
				Routes:    Routes{Match("/*").ProxyTo("api").RedirectTo("/new", 301)},
			},
			"more than one of ProxyTo, Serve, RedirectTo, and Handle",
		},
		{
			"redirect plus serve",
			Config{
				Listeners: Listeners{HTTP(":0")},
				Routes:    Routes{Match("/*").Serve("./public").RedirectTo("/new", 301)},
			},
			"more than one of ProxyTo, Serve, RedirectTo, and Handle",
		},
		{
			"no action at all",
			Config{
				Listeners: Listeners{HTTP(":0")},
				Routes:    Routes{Match("/*")},
			},
			"none of ProxyTo, Serve, RedirectTo, or Handle",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := Resolve(c.cfg)
			if err == nil {
				t.Fatalf("want error containing %q", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("got %q, want substring %q", err, c.wantErr)
			}
		})
	}
}

// redirectRouter resolves cfg and returns the router, shutting down any pools.
func redirectRouter(t *testing.T, cfg Config) http.Handler {
	t.Helper()
	srv, err := newServer(mustResolve(t, cfg))
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	t.Cleanup(func() {
		for _, ph := range srv.pools {
			ph.transport.CloseIdleConnections()
		}
	})
	return srv.buildRouter()
}

// TestRedirectRoute — a fixed target answers with the configured status and
// Location, for each allowed status.
func TestRedirectRoute(t *testing.T) {
	t.Parallel()
	for _, status := range []int{301, 302, 303, 307, 308} {
		router := redirectRouter(t, redirectConfig("https://new.example.com/landing", status))
		rec := runRequest(t, router, httptest.NewRequest("GET", "http://old.example.com/anything", nil))
		if rec.Code != status {
			t.Errorf("status: got %d, want %d", rec.Code, status)
		}
		assertHeader(t, rec.Header(), "Location", "https://new.example.com/landing")
	}
}

// TestRedirectRoutePlaceholders — each placeholder substitutes the matching
// piece of the live request.
func TestRedirectRoutePlaceholders(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		target string
		url    string
		want   string
	}{
		{"request_uri", "https://new.example.com{request_uri}", "http://old.example.com/a/b?x=1&y=2", "https://new.example.com/a/b?x=1&y=2"},
		{"request_uri without query", "https://new.example.com{request_uri}", "http://old.example.com/a/b", "https://new.example.com/a/b"},
		{"path only drops the query", "https://new.example.com{path}", "http://old.example.com/a/b?x=1", "https://new.example.com/a/b"},
		{"query recombined", "/search?{query}&from=old", "http://old.example.com/find?q=x", "/search?q=x&from=old"},
		{"host carried, port stripped", "https://{host}/moved{path}", "http://old.example.com:8080/a", "https://old.example.com/moved/a"},
		{"escaped path stays escaped", "https://new.example.com{path}", "http://old.example.com/a%2Fb", "https://new.example.com/a%2Fb"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			router := redirectRouter(t, redirectConfig(c.target, http.StatusPermanentRedirect))
			rec := runRequest(t, router, httptest.NewRequest("GET", c.url, nil))
			if rec.Code != http.StatusPermanentRedirect {
				t.Fatalf("status: got %d, want 308", rec.Code)
			}
			assertHeader(t, rec.Header(), "Location", c.want)
		})
	}
}

// TestRedirectRouteProtocolRelative — a client-controlled placeholder that
// would make the Location protocol-relative ("//evil.com") is collapsed to a
// same-origin absolute path, so the redirect cannot be steered off-site. This
// covers the raw client path directly and the StripPrefix-induced form.
func TestRedirectRouteProtocolRelative(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  Config
		url  string
		want string
	}{
		{
			// The pre-existing vector: a bare "//evil.com" request path on a
			// {path} redirect, no rewrite involved.
			name: "raw client path via {path}",
			cfg:  redirectConfig("{path}", http.StatusFound),
			url:  "http://old.example.com//evil.com/x",
			want: "/evil.com/x",
		},
		{
			name: "raw client path via {request_uri}",
			cfg:  redirectConfig("{request_uri}", http.StatusFound),
			url:  "http://old.example.com//evil.com/x?a=1",
			want: "/evil.com/x?a=1",
		},
		{
			// StripPrefix turns "/api//evil.com" into "//evil.com".
			name: "strip-prefix induced",
			cfg: Config{
				Listeners: Listeners{HTTP(":0")},
				Routes: Routes{
					Match("/api/*").RedirectTo("{path}", http.StatusFound).With(StripPrefix("/api")),
				},
			},
			url:  "http://old.example.com/api//evil.com/x",
			want: "/evil.com/x",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			router := redirectRouter(t, c.cfg)
			rec := runRequest(t, router, httptest.NewRequest("GET", c.url, nil))
			if rec.Code != http.StatusFound {
				t.Fatalf("status: got %d, want 302", rec.Code)
			}
			assertHeader(t, rec.Header(), "Location", c.want)
		})
	}
}

// TestSafeRedirectLocation — the unit boundary: only a "//"/"/\"-leading value
// is collapsed; scheme, single-slash, and mid-string doubled slashes are kept.
func TestSafeRedirectLocation(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"//evil.com/x", "/evil.com/x"},
		{"/\\evil.com", "/evil.com"},
		{"///evil.com", "/evil.com"},
		{"https://new.example.com/x", "https://new.example.com/x"},
		{"/users", "/users"},
		{"/a//b", "/a//b"},
		{"", ""},
		{"/", "/"},
	}
	for _, c := range cases {
		if got := safeRedirectLocation(c.in); got != c.want {
			t.Errorf("safeRedirectLocation(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestRedirectRouteNoRescan — placeholder-shaped text arriving in the request
// is substituted literally, never expanded a second time.
func TestRedirectRouteNoRescan(t *testing.T) {
	t.Parallel()
	router := redirectRouter(t, redirectConfig("/new?{query}", http.StatusFound))
	rec := runRequest(t, router, httptest.NewRequest("GET", "http://x/a?next={host}", nil))
	assertHeader(t, rec.Header(), "Location", "/new?next={host}")
}

// TestRedirectRouteIssueExample — the declaration from issue #22, verbatim:
// a host-scoped permanent redirect preserving the request URI, beside a
// proxy route the host scope must not capture.
func TestRedirectRouteIssueExample(t *testing.T) {
	t.Parallel()
	backend := newEchoBackend(t)
	router := redirectRouter(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Upstreams: Upstreams{
			"api": Pool{Backends: []Backend{{Address: strings.TrimPrefix(backend.URL, "http://")}}},
		},
		Routes: Routes{
			Match("/*").Host("old.example.com").RedirectTo(
				"https://new.example.com{request_uri}",
				http.StatusPermanentRedirect,
			),
			Match("/*").ProxyTo("api"),
		},
	})

	rec := runRequest(t, router, httptest.NewRequest("GET", "http://old.example.com/docs?page=2", nil))
	if rec.Code != http.StatusPermanentRedirect {
		t.Fatalf("old host status: got %d, want 308", rec.Code)
	}
	assertHeader(t, rec.Header(), "Location", "https://new.example.com/docs?page=2")

	rec = runRequest(t, router, httptest.NewRequest("GET", "http://api.example.com/docs", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("other host status: got %d, want 200 from the proxy route", rec.Code)
	}
}

// TestRedirectRouteWithMiddleware — a redirect route sits behind the same
// middleware chain as any other action.
func TestRedirectRouteWithMiddleware(t *testing.T) {
	t.Parallel()
	router := redirectRouter(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Routes: Routes{
			Match("/*").RedirectTo("/new", http.StatusSeeOther).With(
				SetResponseHeader("Cache-Control", "no-store"),
			),
		},
	})
	rec := runRequest(t, router, httptest.NewRequest("GET", "http://x/old", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, want 303", rec.Code)
	}
	assertHeader(t, rec.Header(), "Location", "/new")
	assertHeader(t, rec.Header(), "Cache-Control", "no-store")
}

// TestParseRedirectTargetSegments — the compiler keeps literals and
// placeholders in order and treats a lone "}" as an ordinary character.
func TestParseRedirectTargetSegments(t *testing.T) {
	t.Parallel()
	segs, err := parseRedirectTarget("https://{host}/x}y{path}")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := ""
	for _, s := range segs {
		if s.placeholder != "" {
			got += fmt.Sprintf("<%s>", s.placeholder)
		} else {
			got += s.literal
		}
	}
	if want := "https://<host>/x}y<path>"; got != want {
		t.Errorf("segments: got %q, want %q", got, want)
	}
}
