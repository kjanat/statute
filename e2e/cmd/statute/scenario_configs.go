//go:build e2e

package main

import statute "statute.kjanat.dev"

// Shared fragments of the scenario configs. Every scenario keeps the
// conventional ports (:8080 http, :8443 https, :8081 health) so the
// harness addresses all nodes uniformly.

// The base stack's origin actors and the pool names scenarios give them.
const (
	origin1Addr = "origin-1:7000"
	origin2Addr = "origin-2:7000"
	poolOrigin  = "origin"
	poolOrigins = "origins"
	healthPath  = "/health"
)

func e2eDefaults() statute.Defaults {
	return statute.Defaults{
		ReadHeaderTimeout: "5s",
		WriteTimeout:      "60s",
		IdleTimeout:       "60s",
	}
}

func e2eShutdown() statute.Shutdown {
	return statute.Shutdown{GracePeriod: "10s", DrainListeners: true}
}

func e2eObservability() statute.Observability {
	return statute.Observability{
		AccessLog: statute.JSONLog(statute.Stdout),
		Health:    statute.Health(":8081", "/healthz"),
	}
}

// routesConfig proves route matching on the original path and
// exactly-once path rewriting across Retry re-entry. The /api route
// strips its prefix ahead of a Retry; the catch-all applies no rewrite.
// A single origin keeps the journal a complete record of attempts.
func routesConfig(string) statute.Config {
	return statute.Config{
		Listeners: statute.Listeners{statute.HTTP(":8080")},
		Upstreams: statute.Upstreams{
			poolOrigin: statute.Pool{
				Backends: []statute.Backend{{Address: origin1Addr}},
			},
		},
		Routes: statute.Routes{
			statute.Match("/api/*").ProxyTo(poolOrigin).
				With(
					statute.StripPrefix("/api"),
					statute.Retry(3, statute.OnStatus(502)),
					statute.RequestID().From("X-Request-Id"),
				),
			statute.Match("/*").ProxyTo(poolOrigin).
				With(statute.RequestID().From("X-Request-Id")),
		},
		Defaults:      e2eDefaults(),
		Observability: e2eObservability(),
		Shutdown:      e2eShutdown(),
	}
}

// upstreamTLSConfig proves upstream TLS verification and Host policy
// parity between proxy traffic and active health probes. The good pool
// verifies against the lane CA; the bad pool trusts only an unrelated
// CA (pinning the served leaf itself would legitimately verify), so
// verification can never succeed and the route must fail closed.
func upstreamTLSConfig(string) statute.Config {
	return statute.Config{
		Listeners: statute.Listeners{statute.HTTP(":8080")},
		Upstreams: statute.Upstreams{
			"tls-good": statute.Pool{
				Backends: []statute.Backend{{Address: "https://" + origin1Addr}},
				HealthCheck: statute.HealthCheck{
					Path:      healthPath,
					Interval:  "2s",
					Timeout:   "1s",
					Healthy:   1,
					Unhealthy: 10,
				},
				Transport:    statute.Transport{RootCAFiles: []string{"/certs/ca.crt"}},
				UpstreamHost: statute.TargetHost,
			},
			"tls-bad": statute.Pool{
				Backends:  []statute.Backend{{Address: "https://" + origin2Addr}},
				Transport: statute.Transport{RootCAFiles: []string{"/certs/wrong-ca.crt"}},
			},
		},
		Routes: statute.Routes{
			statute.Match("/good/*").ProxyTo("tls-good").
				With(statute.StripPrefix("/good"), statute.RequestID().From("X-Request-Id")),
			statute.Match("/bad/*").ProxyTo("tls-bad").
				With(statute.StripPrefix("/bad")),
		},
		Defaults:      e2eDefaults(),
		Observability: e2eObservability(),
		Shutdown:      e2eShutdown(),
	}
}

// h3Config serves the mesh pool over HTTP/1.1, HTTP/2, and HTTP/3; the
// QUIC listener shares the HTTPS port number on UDP so the shutdown
// scenario can prove UDP release at a known address.
func h3Config(string) statute.Config {
	return statute.Config{
		Listeners: statute.Listeners{
			statute.HTTP(":8080"),
			statute.HTTPS(":8443",
				statute.StaticTLS("/certs/statute.crt", "/certs/statute.key"),
				statute.HTTP2(),
				statute.HTTP3(":8443/udp"),
			),
		},
		Upstreams: statute.Upstreams{
			poolOrigins: statute.Pool{
				Backends: []statute.Backend{
					{Address: origin1Addr},
					{Address: origin2Addr},
				},
				Strategy: statute.RoundRobin,
			},
		},
		Routes: statute.Routes{
			statute.Match("/*").ProxyTo(poolOrigins).
				With(statute.RequestID().From("X-Request-Id")),
		},
		Defaults:      e2eDefaults(),
		Observability: e2eObservability(),
		Shutdown:      e2eShutdown(),
	}
}

// isolationConfig gives each node passive health only, fed exclusively
// by the proxy traffic that node itself serves. Failures driven through
// one node demote a backend there and nowhere else — the per-node state
// divergence the isolation scenario asserts.
func isolationConfig(string) statute.Config {
	return statute.Config{
		Listeners: statute.Listeners{statute.HTTP(":8080")},
		Upstreams: statute.Upstreams{
			poolOrigins: statute.Pool{
				Backends: []statute.Backend{
					{Address: origin1Addr},
					{Address: origin2Addr},
				},
				Strategy: statute.RoundRobin,
				PassiveHealthCheck: statute.PassiveHealthCheck{
					FailureWindow: "30s",
					MaxFailures:   3,
				},
			},
		},
		Routes: statute.Routes{
			statute.Match("/*").ProxyTo(poolOrigins).
				With(statute.RequestID().From("X-Request-Id")),
		},
		Defaults:      e2eDefaults(),
		Observability: e2eObservability(),
		Shutdown:      e2eShutdown(),
	}
}

// startupBadConfig references TLS material that does not exist, so
// Start must fail, the process must exit non-zero, and no route may
// ever be exposed. The corrected retry is a fresh container running the
// mesh scenario in the same service slot.
func startupBadConfig(string) statute.Config {
	return statute.Config{
		Listeners: statute.Listeners{
			statute.HTTP(":8080"),
			statute.HTTPS(":8443",
				statute.StaticTLS("/certs/missing.crt", "/certs/missing.key"),
			),
		},
		Upstreams: statute.Upstreams{
			poolOrigin: statute.Pool{
				Backends: []statute.Backend{{Address: origin1Addr}},
			},
		},
		Routes:        statute.Routes{statute.Match("/*").ProxyTo(poolOrigin)},
		Defaults:      e2eDefaults(),
		Observability: e2eObservability(),
		Shutdown:      e2eShutdown(),
	}
}

// trustedConfig exposes the same route on two listeners that differ in
// trusted-proxy policy, so the access log's client attribution can be
// compared for a spoofed forwarded header from an untrusted peer
// (plain :8080; TrustedProxy is an HTTPS listener option) versus a
// trusted one (:8443).
func trustedConfig(string) statute.Config {
	return statute.Config{
		Listeners: statute.Listeners{
			statute.HTTP(":8080"),
			statute.HTTPS(":8443",
				statute.StaticTLS("/certs/statute.crt", "/certs/statute.key"),
				statute.TrustedProxy("0.0.0.0/0"),
			),
		},
		Upstreams: statute.Upstreams{
			poolOrigin: statute.Pool{
				Backends: []statute.Backend{{Address: origin1Addr}},
			},
		},
		Routes: statute.Routes{
			statute.Match("/*").ProxyTo(poolOrigin).
				With(statute.RequestID().From("X-Request-Id")),
		},
		Defaults:      e2eDefaults(),
		Observability: e2eObservability(),
		Shutdown:      e2eShutdown(),
	}
}
