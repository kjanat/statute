package statute

import (
	"strings"
	"testing"
	"time"

	"statute.kjanat.dev/resolved"
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

// poolConfig wraps one pool in an otherwise-valid Config.
func poolConfig(p Pool) Config {
	return Config{
		Listeners: Listeners{HTTP(":8080")},
		Upstreams: Upstreams{"api": p},
		Routes:    Routes{Match("/*").ProxyTo("api")},
	}
}

func TestResolveHealthCheckStatuses(t *testing.T) {
	t.Parallel()
	okBackends := []Backend{{Address: "127.0.0.1:9001"}}

	r, err := Resolve(poolConfig(Pool{
		Backends:    okBackends,
		HealthCheck: HealthCheck{Path: "/h", Statuses: []int{200, 204}},
	}))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got := r.Upstreams["api"].HealthCheck.Statuses
	if len(got) != 2 || got[0] != 200 || got[1] != 204 {
		t.Errorf("resolved Statuses: %v", got)
	}

	for _, bad := range []int{0, 99, 600, -1} {
		_, err := Resolve(poolConfig(Pool{
			Backends:    okBackends,
			HealthCheck: HealthCheck{Path: "/h", Statuses: []int{200, bad}},
		}))
		if err == nil || !strings.Contains(err.Error(), "statuses[1]") {
			t.Errorf("status %d: got %v, want statuses[1] error", bad, err)
		}
	}
}

// TestResolveProbeFieldsRequirePath — the new probe fields describe active
// probing, so setting them without a Path is a policy drop and fails
// Resolve. Passive health is independent of active probing and stays legal
// with an empty Path.
func TestResolveProbeFieldsRequirePath(t *testing.T) {
	t.Parallel()
	okBackends := []Backend{{Address: "127.0.0.1:9001"}}

	_, err := Resolve(poolConfig(Pool{
		Backends:    okBackends,
		HealthCheck: HealthCheck{Host: "api.internal"},
	}))
	if err == nil || !strings.Contains(err.Error(), "host: set but path is empty") {
		t.Errorf("Host without Path: got %v", err)
	}

	_, err = Resolve(poolConfig(Pool{
		Backends:    okBackends,
		HealthCheck: HealthCheck{Statuses: []int{200}},
	}))
	if err == nil || !strings.Contains(err.Error(), "statuses: set but path is empty") {
		t.Errorf("Statuses without Path: got %v", err)
	}

	r, err := Resolve(poolConfig(Pool{
		Backends:           okBackends,
		PassiveHealthCheck: PassiveHealthCheck{FailureWindow: "30s", MaxFailures: 3},
	}))
	if err != nil {
		t.Fatalf("passive without Path: %v", err)
	}
	if !r.Upstreams["api"].PassiveHealthCheck.Enabled {
		t.Error("passive without Path did not resolve enabled")
	}
}

func TestResolvePassiveHealthCheck(t *testing.T) {
	t.Parallel()
	okBackends := []Backend{{Address: "127.0.0.1:9001"}}
	resolve := func(p PassiveHealthCheck) (*resolved.PassiveHealthCheck, error) {
		r, err := Resolve(poolConfig(Pool{Backends: okBackends, PassiveHealthCheck: p}))
		if err != nil {
			return nil, err
		}
		phc := r.Upstreams["api"].PassiveHealthCheck
		return &phc, nil
	}

	phc, err := resolve(PassiveHealthCheck{})
	if err != nil || phc.Enabled {
		t.Errorf("zero value: got %+v, %v, want disabled", phc, err)
	}

	phc, err = resolve(PassiveHealthCheck{FailureWindow: "30s", MaxFailures: 3})
	if err != nil {
		t.Fatalf("valid: %v", err)
	}
	if !phc.Enabled || phc.FailureWindow != 30*time.Second || phc.MaxFailures != 3 {
		t.Errorf("valid: got %+v", phc)
	}
}

// TestResolvePassiveHealthCheckErrors — a half-set or unparsable policy is
// rejected rather than guessed, under the passive_health_check prefix.
func TestResolvePassiveHealthCheckErrors(t *testing.T) {
	t.Parallel()
	okBackends := []Backend{{Address: "127.0.0.1:9001"}}
	cases := map[string]struct {
		cfg  PassiveHealthCheck
		want string
	}{
		"window only":      {PassiveHealthCheck{FailureWindow: "30s"}, "set together"},
		"failures only":    {PassiveHealthCheck{MaxFailures: 3}, "set together"},
		"bad window":       {PassiveHealthCheck{FailureWindow: "nope", MaxFailures: 3}, "failure_window:"},
		"zero window":      {PassiveHealthCheck{FailureWindow: "0s", MaxFailures: 3}, "failure_window:"},
		"negative counter": {PassiveHealthCheck{FailureWindow: "30s", MaxFailures: -1}, "MaxFailures is negative"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Resolve(poolConfig(Pool{Backends: okBackends, PassiveHealthCheck: tc.cfg}))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want substring %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "passive_health_check") {
				t.Errorf("error %q lacks the passive_health_check prefix", err)
			}
		})
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
		{"transport.flush_interval", withPool(Pool{Backends: okBackends, Strategy: RoundRobin, Transport: Transport{FlushInterval: bad}}), "flush_interval:"},
		{"transport.flush_interval_negative", withPool(Pool{Backends: okBackends, Strategy: RoundRobin, Transport: Transport{FlushInterval: "-100ms"}}), "flush_interval:"},
		{"transport.flush_interval_minus_one", withPool(Pool{Backends: okBackends, Strategy: RoundRobin, Transport: Transport{FlushInterval: "-1"}}), "flush_interval:"},
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

// badHealthEndpoint is a foreign HealthEndpoint implementation Resolve
// must reject rather than silently dropping the requested endpoint.
type badHealthEndpoint struct{}

func (badHealthEndpoint) statuteHealth() {}

func TestResolveHealthValidation(t *testing.T) {
	t.Parallel()
	base := func() Config {
		return Config{
			Listeners: Listeners{HTTP(":8080")},
			Upstreams: Upstreams{"api": Pool{Backends: []Backend{{Address: "127.0.0.1:9001"}}}},
			Routes:    Routes{Match("/*").ProxyTo("api")},
		}
	}

	cases := []struct {
		name    string
		health  HealthEndpoint
		want    resolved.Health
		wantErr string
	}{
		{
			name:    "empty addr errors",
			health:  Health("", "/healthz"),
			wantErr: "health: addr required",
		},
		{
			name:   "empty path defaults",
			health: Health("127.0.0.1:9091", ""),
			want:   resolved.Health{Enabled: true, Addr: "127.0.0.1:9091", Path: "/healthz"},
		},
		{
			name:   "explicit path round-trips",
			health: Health("127.0.0.1:9091", "/livez"),
			want:   resolved.Health{Enabled: true, Addr: "127.0.0.1:9091", Path: "/livez"},
		},
		{
			name:   "absent stays disabled",
			health: nil,
			want:   resolved.Health{},
		},
		{
			name:    "path without leading slash errors",
			health:  Health("127.0.0.1:9091", "healthz"),
			wantErr: `health: path "healthz" must start with /`,
		},
		{
			name:    "root path errors",
			health:  Health("127.0.0.1:9091", "/"),
			wantErr: `health: path "/" is not allowed`,
		},
		{
			name:    "double leading slash errors",
			health:  Health("127.0.0.1:9091", "//healthz"),
			wantErr: `health: path "//healthz" must start with a single /`,
		},
		{
			name:    "backslash after slash errors",
			health:  Health("127.0.0.1:9091", `/\healthz`),
			wantErr: `health: path "/\\healthz" must start with a single /`,
		},
		{
			name:    "trailing slash errors",
			health:  Health("127.0.0.1:9091", "/healthz/"),
			wantErr: `health: path "/healthz/" must not end with /`,
		},
		{
			name:    "unknown marker type errors",
			health:  badHealthEndpoint{},
			wantErr: "unknown health type",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			cfg.Observability = Observability{Health: tc.health}
			r, err := Resolve(cfg)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if r.Observability.Health != tc.want {
				t.Errorf("resolved health = %+v, want %+v", r.Observability.Health, tc.want)
			}
		})
	}
}
