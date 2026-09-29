//go:build e2e

package main

import (
	"fmt"
	"os"
	"strings"

	statute "statute.kjanat.dev"
)

// acmeHTTP01Config issues a real certificate for proxy.e2e.test from
// Pebble over the HTTP-01 challenge, hermetically: the Directory knob
// points the pinned in-tree manager at Pebble's directory, the plain
// listener serves the challenge tokens, and Pebble reaches it because
// the statute service carries a network alias equal to the domain.
// Pebble's HTTPS is trusted via SSL_CERT_FILE in the container env.
func acmeHTTP01Config(string) statute.Config {
	return statute.Config{
		Listeners: statute.Listeners{
			statute.HTTP(":8080"),
			statute.HTTPS(":8443",
				statute.AutoTLS("proxy.e2e.test").
					Email("e2e@example.test").
					Storage("/var/lib/statute/acme").
					HTTP01().
					Directory("https://pebble:14000/dir"),
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

// dockerDiscoveryConfig discovers labeled containers from a real Docker
// Engine (the daemon socket is mounted by the scenario override;
// discovery is opt-in per container label, so the lane's own
// infrastructure never surfaces as routes). Code-owned pool policy must
// reach the project-scoped native service, and the static route must keep
// shadowing its label-derived catch-all.
func dockerDiscoveryConfig(string) statute.Config {
	service := os.Getenv("STATUTE_DISCOVERY_SERVICE")
	if strings.TrimSpace(service) == "" {
		fmt.Fprintln(os.Stderr, "statute-e2e: STATUTE_DISCOVERY_SERVICE is required for the docker scenario")
		os.Exit(2)
	}
	return statute.Config{
		Listeners: statute.Listeners{statute.HTTP(":8080")},
		Upstreams: statute.Upstreams{
			"static-origin": statute.Pool{
				Backends: []statute.Backend{{Address: origin1Addr}},
			},
		},
		Routes: statute.Routes{
			statute.Match("/static/*").ProxyTo("static-origin").
				With(statute.StripPrefix("/static"), statute.RequestID().From("X-Request-Id")),
		},
		FallbackRoutes: statute.Routes{
			statute.Match("/*").Host(strings.TrimSuffix(service, "-dyn")+".test").
				ProxyTo("static-origin").With(statute.ReplacePath("/echo"), statute.SetRequestHeader("X-Request-Id", "terminal-route")),
		},
		Docker: statute.Docker().Refresh("1s").PoolPolicy(service, statute.PoolPolicy{
			HealthCheck:        statute.HealthCheck{Path: healthPath, Interval: "2s", Healthy: 1},
			PassiveHealthCheck: statute.PassiveHealthCheck{FailureWindow: "30s", MaxFailures: 3},
			Transport:          statute.Transport{ResponseHeaderTimeout: "5s"},
			UpstreamHost:       statute.HostValue("policy.internal"),
		}),
		Defaults:      e2eDefaults(),
		Observability: e2eObservability(),
		Shutdown:      e2eShutdown(),
	}
}

// workloadConfig grants on-demand activation for a project-scoped service:
// its labeled container is started by routed demand, adopted when found
// running, and stopped again after a short idle window. Readiness stays on
// the automatic TCP probe, and discovery uses events without periodic polling.
func workloadConfig(string) statute.Config {
	observability := e2eObservability()
	observability.Metrics = statute.Prometheus(":9090", "/metrics")
	return statute.Config{
		Listeners: statute.Listeners{statute.HTTP(":8080")},
		Docker: statute.Docker().Storage("/var/lib/statute/docker").Workload(os.Getenv("STATUTE_WORKLOAD_SERVICE"), statute.WorkloadPolicy{
			IdleAfter: "10s",
		}),
		Defaults:      e2eDefaults(),
		Observability: observability,
		Shutdown:      e2eShutdown(),
	}
}

// workloadConcurrencyConfig holds cold traffic behind an explicitly controlled
// HTTP readiness gate while the recorder observes real Docker start requests.
func workloadConcurrencyConfig(string) statute.Config {
	observability := e2eObservability()
	observability.Metrics = statute.Prometheus(":9090", "/metrics")
	return statute.Config{
		Listeners: statute.Listeners{statute.HTTP(":8080")},
		Docker: statute.Docker().Endpoint("tcp://dockerproxy:2375").Storage("/var/lib/statute/docker").Workload(os.Getenv("STATUTE_WORKLOAD_SERVICE"), statute.WorkloadPolicy{
			IdleAfter: "5m", ReadyTimeout: "1m", Readiness: statute.HTTPReadiness("/health"),
		}),
		Defaults: e2eDefaults(), Observability: observability, Shutdown: e2eShutdown(),
	}
}

// observabilityConfig emits all three signals plus the health endpoint
// so one request id can be correlated from the client report through
// the access log and origin journal to a span at the collector and a
// metrics scrape.
func observabilityConfig(string) statute.Config {
	return statute.Config{
		Listeners: statute.Listeners{statute.HTTP(":8080")},
		Upstreams: statute.Upstreams{
			poolOrigin: statute.Pool{
				Backends: []statute.Backend{{Address: origin1Addr}},
			},
		},
		Routes: statute.Routes{
			statute.Match("/*").ProxyTo(poolOrigin).
				With(statute.RequestID().From("X-Request-Id")),
		},
		Defaults: e2eDefaults(),
		Observability: statute.Observability{
			AccessLog: statute.JSONLog(statute.Stdout),
			Metrics:   statute.Prometheus(":9090", "/metrics"),
			Tracing:   statute.OTLP("otelcol:4317").Insecure().ServiceName("statute-e2e"),
			Health:    statute.Health(":8081", "/healthz"),
		},
		Shutdown: e2eShutdown(),
	}
}
