package statute

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"statute.kjanat.dev/resolved"
)

// clientIPRouter builds a router with the issue's two-route shape: a
// trusted-network route serving "inside", and an unconstrained fallback
// serving "outside".
func clientIPRouter(t *testing.T) http.Handler {
	t.Helper()
	inside, outside := t.TempDir(), t.TempDir()
	writeFile(t, inside, "index.html", "inside")
	writeFile(t, outside, "index.html", "outside")
	r := mustResolve(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Routes: Routes{
			Match("/*").Host("admin.example.com").ClientIPs("10.0.0.0/8").Serve(inside),
			Match("/*").Host("admin.example.com").Serve(outside),
		},
	})
	srv, err := newServer(r)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	t.Cleanup(func() {
		for _, ph := range srv.pools {
			ph.shutdown()
		}
	})
	return srv.buildRouter()
}

// TestClientIPsRouteFallThrough — the constraint is a matcher, not a
// gate: an outside client falls through to the next route instead of
// receiving the 403 AllowIPs middleware would produce.
func TestClientIPsRouteFallThrough(t *testing.T) {
	t.Parallel()
	router := clientIPRouter(t)
	cases := []struct {
		name   string
		remote string
		want   string
	}{
		{"trusted network takes the first route", "10.1.2.3:4321", "inside"},
		{"outside client falls through", "198.51.100.1:4321", "outside"},
		{"unparsable client never matches a constrained route", "pipe", "outside"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest("GET", "http://admin.example.com/", nil)
			req.RemoteAddr = c.remote
			rec := runRequest(t, router, req)
			if rec.Code != http.StatusOK || rec.Body.String() != c.want {
				t.Errorf("got %d %q, want 200 %q", rec.Code, rec.Body.String(), c.want)
			}
		})
	}
}

// TestClientIPsRouteNoFallback — with only the constrained route declared,
// an outside client gets the router's 404, proving the constraint belongs
// to matching rather than to the route's handler.
func TestClientIPsRouteNoFallback(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "index.html", "inside")
	r := mustResolve(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Routes:    Routes{Match("/*").ClientIPs("10.0.0.0/8").Serve(dir)},
	})
	srv, err := newServer(r)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	req := httptest.NewRequest("GET", "http://x/", nil)
	req.RemoteAddr = "198.51.100.1:4321"
	if rec := runRequest(t, srv.buildRouter(), req); rec.Code != http.StatusNotFound {
		t.Errorf("outside client: got %d, want 404", rec.Code)
	}
}

// TestClientIPsUseVerifiedResolution — the matcher keys on the same
// clientIP the rate limiter uses: under a TrustedProxy listener policy, a
// trusted peer's forwarded assertion selects the trusted-network route,
// while an untrusted peer spoofing the same header falls through.
func TestClientIPsUseVerifiedResolution(t *testing.T) {
	t.Parallel()
	router := clientIPRouter(t)
	l := &resolved.Listener{
		TrustedProxies: []string{"203.0.113.0/24"},
		ClientIPHeader: "X-Forwarded-For",
	}
	handler := trustedProxyMiddleware(l, router)

	req := httptest.NewRequest("GET", "http://admin.example.com/", nil)
	req.RemoteAddr = "203.0.113.7:4321"
	req.Header.Set("X-Forwarded-For", "10.0.0.1")
	if rec := runRequest(t, handler, req); rec.Body.String() != "inside" {
		t.Errorf("trusted assertion: got %q, want %q", rec.Body.String(), "inside")
	}

	req = httptest.NewRequest("GET", "http://admin.example.com/", nil)
	req.RemoteAddr = "198.51.100.1:4321"
	req.Header.Set("X-Forwarded-For", "10.0.0.1")
	if rec := runRequest(t, handler, req); rec.Body.String() != "outside" {
		t.Errorf("spoofed assertion: got %q, want %q", rec.Body.String(), "outside")
	}
}

// TestResolveClientIPs — CIDRs are canonicalised into the resolved schema,
// and bad declarations fail at resolve time.
func TestResolveClientIPs(t *testing.T) {
	t.Parallel()
	r := mustResolve(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Routes:    Routes{Match("/*").ClientIPs("10.0.0.5/8", "2001:db8::1/32").Serve("./public")},
	})
	got := r.Routes[0].ClientIPCIDRs
	if len(got) != 2 || got[0] != "10.0.0.0/8" || got[1] != "2001:db8::/32" {
		t.Errorf("canonical CIDRs: %v", got)
	}
	if r.Routes[0].Host != "" || r.Routes[0].StaticDir == "" {
		t.Errorf("route: %+v", r.Routes[0])
	}

	cases := []struct {
		name    string
		route   *Route
		wantErr string
	}{
		{"explicitly empty", Match("/*").ClientIPs().Serve("./public"), "at least one CIDR"},
		{"bad cidr", Match("/*").ClientIPs("not-a-cidr").Serve("./public"), "client_ips"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := Resolve(Config{
				Listeners: Listeners{HTTP(":0")},
				Routes:    Routes{c.route},
			})
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("got %v, want substring %q", err, c.wantErr)
			}
		})
	}
}
