// Example: a minimal plain-HTTP reverse proxy with no TLS — a single
// :8080 listener round-robining across two echo backends, with per-route
// rate limiting and timeouts, JSON access logging, Prometheus metrics, and
// graceful shutdown. Useful for local development or running behind a TLS
// terminator that handles certificates upstream.
package main

import "statute.kjanat.dev"

func main() {
	statute.Run(statute.Config{
		Listeners: statute.Listeners{
			statute.HTTP(":8080"),
		},

		Upstreams: statute.Upstreams{
			"echo": statute.Pool{
				Backends: []statute.Backend{
					{Address: "127.0.0.1:9001"},
					{Address: "127.0.0.1:9002"},
				},
				Strategy: statute.RoundRobin,
				Transport: statute.Transport{
					MaxIdleConnsPerHost: 8,
					IdleConnTimeout:     "60s",
				},
			},
		},

		Routes: statute.Routes{
			statute.Match("/api/*").ProxyTo("echo").
				With(
					statute.RateLimit("100/s").Per(statute.ClientIP),
					statute.Timeout("10s"),
				),
			statute.Match("/*").ProxyTo("echo").
				With(statute.Timeout("10s")),
		},

		Defaults: statute.Defaults{
			ReadHeaderTimeout: "5s",
			WriteTimeout:      "30s",
			IdleTimeout:       "120s",
		},

		Observability: statute.Observability{
			AccessLog: statute.JSONLog(statute.Stdout),
			Metrics:   statute.Prometheus(":9090", "/metrics"),
		},

		Shutdown: statute.Shutdown{
			GracePeriod:    "10s",
			DrainListeners: true,
		},
	})
}
