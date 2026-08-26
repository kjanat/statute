//go:build e2e

package main

import statute "statute.kjanat.dev"

// meshConfig is the smoke scenario: plain HTTP and TLS/HTTP2 listeners
// in front of a round-robin pool over both origins, with active health,
// Retry for the failover path, and the health endpoint the harness
// gates readiness on. Both nodes of a two-server topology run this same
// config with fully isolated state.
func meshConfig(string) statute.Config {
	return statute.Config{
		Listeners: statute.Listeners{
			statute.HTTP(":8080"),
			statute.HTTPS(":8443",
				statute.StaticTLS("/certs/statute.crt", "/certs/statute.key"),
				statute.HTTP2(),
			),
		},
		Upstreams: statute.Upstreams{
			"origins": statute.Pool{
				Backends: []statute.Backend{
					{Address: "origin-1:7000"},
					{Address: "origin-2:7000"},
				},
				Strategy: statute.RoundRobin,
				HealthCheck: statute.HealthCheck{
					Path:      "/health",
					Interval:  "2s",
					Timeout:   "1s",
					Healthy:   1,
					Unhealthy: 2,
				},
			},
		},
		Routes: statute.Routes{
			// Retry(3) covers the smoke failover path: with a per-key
			// failure budget armed on each origin independently, three
			// attempts guarantee one lands on an origin whose budget is
			// already spent.
			statute.Match("/*").ProxyTo("origins").
				With(
					statute.Retry(3, statute.OnStatus(502)),
					// From lets the client-stamped id survive to the origin,
					// which is what the smoke identity assertion checks.
					statute.RequestID().From("X-Request-Id"),
					statute.Timeout("30s"),
				),
		},
		Defaults: statute.Defaults{
			ReadHeaderTimeout: "5s",
			WriteTimeout:      "60s",
			IdleTimeout:       "60s",
		},
		Observability: statute.Observability{
			AccessLog: statute.JSONLog(statute.Stdout),
			Health:    statute.Health(":8081", "/healthz"),
		},
		Shutdown: statute.Shutdown{
			GracePeriod:    "10s",
			DrainListeners: true,
		},
	}
}
