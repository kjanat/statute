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
			poolOrigins: statute.Pool{
				Backends: []statute.Backend{
					{Address: origin1Addr},
					{Address: origin2Addr},
				},
				Strategy: statute.RoundRobin,
				HealthCheck: statute.HealthCheck{
					Path:      healthPath,
					Interval:  "2s",
					Timeout:   "1s",
					Healthy:   1,
					Unhealthy: 2,
				},
			},
		},
		Routes: statute.Routes{
			// No Timeout here: http.TimeoutHandler buffers bodies and hides
			// http.Hijacker, breaking the streaming and upgrade scenarios.
			statute.Match("/*").ProxyTo(poolOrigins).
				With(
					// INVARIANT: Retry(3) — /fail arms its budget per origin,
					// so only a third attempt is guaranteed to land on one
					// already spent.
					statute.Retry(3, statute.OnStatus(502)),
					// From lets the client-stamped id survive to the origin,
					// which is what the smoke identity assertion checks.
					statute.RequestID().From("X-Request-Id"),
				),
		},
		Defaults:      e2eDefaults(),
		Observability: e2eObservability(),
		Shutdown:      e2eShutdown(),
	}
}
