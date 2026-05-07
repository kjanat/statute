// Example: wildcard certificate via Cloudflare DNS-01.
//
// HTTP-01 cannot issue wildcard certificates (RFC 8555 §8.4: only DNS-01 is
// permitted for wildcards). This deployment uses Cloudflare's DNS API to
// satisfy DNS-01 challenges and provisions a single cert that covers
// *.example.com plus the apex example.com.
//
// The CLOUDFLARE_API_TOKEN must have Zone.DNS:Edit on the example.com zone.
// Generate one at https://dash.cloudflare.com/profile/api-tokens.
//
// :80 is not required to be reachable for DNS-01, so this listener config
// can run on a private network or behind a firewall.
package main

import (
	"log"
	"os"

	"github.com/kjanat/statute"
)

func main() {
	token := os.Getenv("CLOUDFLARE_API_TOKEN")
	if token == "" {
		log.Fatal("CLOUDFLARE_API_TOKEN env var is required")
	}

	statute.Main(statute.Config{
		Listeners: statute.Listeners{
			statute.HTTPS(":443",
				statute.AutoTLS("*.example.com", "example.com").
					Email("ops@example.com").
					Storage("/var/lib/statute/certs").
					CloudflareDNS01(token),
				statute.HTTP2(),
			),
		},

		Upstreams: statute.Upstreams{
			"api": statute.Pool{
				Backends: []statute.Backend{
					{Address: "10.0.0.1:8080"},
				},
				Strategy: statute.RoundRobin,
			},
		},

		Routes: statute.Routes{
			statute.Match("/*").ProxyTo("api").With(statute.Timeout("30s")),
		},

		Defaults: statute.Defaults{
			ReadHeaderTimeout: "5s",
			WriteTimeout:      "30s",
			IdleTimeout:       "120s",
		},

		Observability: statute.Observability{
			AccessLog: statute.JSONLog(statute.Stdout).Sample(0.1),
			Metrics:   statute.Prometheus(":9090", "/metrics"),
			Tracing:   statute.OTLP("otel-collector:4317").ServiceName("edge-proxy").Insecure(),
		},

		Shutdown: statute.Shutdown{
			GracePeriod:    "30s",
			DrainListeners: true,
		},
	})
}
