// Example: a statute deployment fronted by Cloudflare with origin AutoTLS.
//
// Automatic ACME still attempts TLS-ALPN-01 before HTTP-01 behind Cloudflare.
// Pin HTTP01() to avoid that failed attempt; see docs/cloudflare.md.
//
// The separate TrustedProxy policy accepts CF-Connecting-IP only from
// direct peers in startup-refreshed Cloudflare ranges, refreshed periodically.
//
// Cloudflare-side prerequisites:
//   - SSL/TLS mode: Full (Strict).
//   - "Always Use HTTPS" disabled (or bypassed) for the path
//     /.well-known/acme-challenge/* so HTTP-01 reaches the origin on :80.
//   - WAF rule that does not block requests to the same path.
package main

import "statute.kjanat.dev"

func main() {
	statute.Main(statute.Config{
		Listeners: statute.Listeners{
			statute.HTTP(":80").RedirectTo("https"),
			statute.HTTPS(":443",
				statute.AutoTLS("example.com", "api.example.com").
					Email("ops@example.com").
					Storage("/var/lib/statute/certs"),
				statute.HTTP2(),
				statute.BehindCloudflare(),
				statute.CloudflareTrustedProxy(),
			),
		},

		Upstreams: statute.Upstreams{
			"api": statute.Pool{
				Backends: []statute.Backend{
					{Address: "10.0.0.1:8080"},
					{Address: "10.0.0.2:8080"},
				},
				Strategy: statute.LeastConnections,
				HealthCheck: statute.HealthCheck{
					Path:     "/healthz",
					Interval: "10s",
				},
			},
		},

		Routes: statute.Routes{
			statute.Match("/api/*").ProxyTo("api").
				With(
					// RateLimit keys on the originating client IP. With
					// TrustedProxy this is CF-Connecting-IP only for a verified
					// proxy peer; direct callers cannot forge their bucket.
					statute.RateLimit("100/min").Per(statute.ClientIP),
					statute.Timeout("30s"),
				),
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
			GracePeriod:    "30s",
			DrainListeners: true,
		},
	})
}
