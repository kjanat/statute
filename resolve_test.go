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

// TestResolveParseErrors exercises every parse.* error-rewrap branch in
// resolve.go: each case feeds one invalid duration/rate/size string through
// a Config that is otherwise valid, and asserts Resolve surfaces the
// context-prefixed wrap (e.g. "cache: invalid duration ...").
func TestResolveParseErrors(t *testing.T) {
	const bad = "nope" // invalid as a duration, rate, and size

	base := func() Config {
		return Config{
			Listeners: Listeners{HTTP(":8080")},
			Upstreams: Upstreams{
				"api": Pool{
					Backends: []Backend{{Address: "127.0.0.1:9001"}},
					Strategy: RoundRobin,
				},
			},
			Routes: Routes{Match("/*").ProxyTo("api")},
		}
	}
	withMW := func(mw Middleware) func(*Config) {
		return func(c *Config) {
			c.Routes = Routes{Match("/*").ProxyTo("api").With(mw)}
		}
	}
	withPool := func(p Pool) func(*Config) {
		return func(c *Config) { c.Upstreams = Upstreams{"api": p} }
	}
	okBackends := []Backend{{Address: "127.0.0.1:9001"}}

	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"defaults.read_header_timeout", func(c *Config) { c.Defaults = Defaults{ReadHeaderTimeout: bad} }, "read_header_timeout:"},
		{"defaults.read_timeout", func(c *Config) { c.Defaults = Defaults{ReadTimeout: bad} }, "read_timeout:"},
		{"defaults.write_timeout", func(c *Config) { c.Defaults = Defaults{WriteTimeout: bad} }, "write_timeout:"},
		{"defaults.idle_timeout", func(c *Config) { c.Defaults = Defaults{IdleTimeout: bad} }, "idle_timeout:"},
		{"healthcheck.interval", withPool(Pool{Backends: okBackends, Strategy: RoundRobin, HealthCheck: HealthCheck{Path: "/h", Interval: bad}}), "interval:"},
		{"healthcheck.timeout", withPool(Pool{Backends: okBackends, Strategy: RoundRobin, HealthCheck: HealthCheck{Path: "/h", Timeout: bad}}), "timeout:"},
		{"transport.idle_conn_timeout", withPool(Pool{Backends: okBackends, Strategy: RoundRobin, Transport: Transport{IdleConnTimeout: bad}}), "idle_conn_timeout:"},
		{"transport.dial_timeout", withPool(Pool{Backends: okBackends, Strategy: RoundRobin, Transport: Transport{DialTimeout: bad}}), "dial_timeout:"},
		{"transport.tls_handshake_timeout", withPool(Pool{Backends: okBackends, Strategy: RoundRobin, Transport: Transport{TLSHandshakeTimeout: bad}}), "tls_handshake_timeout:"},
		{"mw.timeout", withMW(Timeout(bad)), "timeout:"},
		{"mw.rate_limit", withMW(RateLimit(bad)), "rate_limit:"},
		{"mw.cache", withMW(Cache(bad)), "cache:"},
		{"mw.body_limit", withMW(BodyLimit(bad)), "body_limit:"},
		{"mw.security_headers.hsts", withMW(SecurityHeaders().HSTS(bad)), "security_headers.hsts:"},
		{"mw.cors.max_age", withMW(CORS().Origins("https://example.com").MaxAge(bad)), "cors.max_age:"},
		{"shutdown.grace_period", func(c *Config) { c.Shutdown = Shutdown{GracePeriod: bad} }, "grace_period:"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.mutate(&cfg)
			_, err := Resolve(cfg)
			if err == nil {
				t.Fatalf("%s: want error, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s: error %q, want substring %q", tc.name, err, tc.want)
			}
		})
	}
}
