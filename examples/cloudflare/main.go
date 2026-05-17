// Example: a statute deployment fronted by Cloudflare with origin AutoTLS.
//
// Cloudflare terminates TLS at its edge and re-encrypts to the origin. This
// origin runs autocert with the HTTP-01 challenge (TLS-ALPN-01 cannot work
// behind Cloudflare because the proxy strips custom ALPN). The
// BehindCloudflare() option does two things:
//
//   - Drops "acme-tls/1" from the listener's ALPN advertisement so autocert
//     does not choose TLS-ALPN-01 and quietly fail.
//   - Marks requests on this listener as trusted-proxy so CF-Connecting-IP
//     and True-Client-IP become the authoritative client IP for rate
//     limiting, access logs, and IP-hash load balancing.
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
					// BehindCloudflare() this is CF-Connecting-IP, not the
					// proxy's address — so each real client gets its own bucket.
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
