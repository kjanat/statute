package statute

import (
	"strings"
	"testing"
)

func TestResolveHappyPath(t *testing.T) {
	cfg := Config{
		Listeners: Listeners{HTTP(":8080")},
		Upstreams: Upstreams{
			"api": Pool{
				Backends: []Backend{
					{Address: "127.0.0.1:9001"},
				},
				Strategy: RoundRobin,
			},
		},
		Routes: Routes{
			Match("/*").ProxyTo("api").With(Timeout("5s")),
		},
	}
	r, err := Resolve(cfg)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := r.Defaults.ReadHeaderTimeout.String(); got != "5s" {
		t.Errorf("default ReadHeaderTimeout: got %v, want 5s", got)
	}
	if r.Routes[0].Upstream == nil {
		t.Fatal("upstream not resolved")
	}
	if r.Routes[0].Upstream.Name != "api" {
		t.Errorf("upstream name: got %q", r.Routes[0].Upstream.Name)
	}
}

func TestResolveUnknownUpstream(t *testing.T) {
	cfg := Config{
		Listeners: Listeners{HTTP(":8080")},
		Routes: Routes{
			Match("/*").ProxyTo("nonexistent"),
		},
	}
	_, err := Resolve(cfg)
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "unknown upstream") {
		t.Errorf("got %q, want 'unknown upstream' substring", err)
	}
}

func TestResolveConflictingRoute(t *testing.T) {
	cfg := Config{
		Listeners: Listeners{HTTP(":8080")},
		Upstreams: Upstreams{
			"api": Pool{Backends: []Backend{{Address: "127.0.0.1:9001"}}},
		},
		Routes: Routes{
			Match("/*").ProxyTo("api").Serve("./public"),
		},
	}
	_, err := Resolve(cfg)
	if err == nil {
		t.Fatal("want error for proxyto+serve conflict")
	}
}

func TestResolveAutoTLSRequiresEmail(t *testing.T) {
	cfg := Config{
		Listeners: Listeners{
			HTTPS(":443", AutoTLS("example.com").Storage("/tmp/certs")),
		},
		Upstreams: Upstreams{
			"api": Pool{Backends: []Backend{{Address: "127.0.0.1:9001"}}},
		},
		Routes: Routes{Match("/*").ProxyTo("api")},
	}
	_, err := Resolve(cfg)
	if err == nil {
		t.Fatal("want error when AutoTLS email missing")
	}
	if !strings.Contains(err.Error(), "email") {
		t.Errorf("got %q, want 'email' substring", err)
	}
}

func TestResolveListenerlessFails(t *testing.T) {
	cfg := Config{
		Upstreams: Upstreams{"api": Pool{Backends: []Backend{{Address: "x:1"}}}},
		Routes:    Routes{Match("/*").ProxyTo("api")},
	}
	_, err := Resolve(cfg)
	if err == nil {
		t.Fatal("want error for missing listener")
	}
}
