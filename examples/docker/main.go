// Example: Docker label discovery — the statute equivalent of running
// Traefik in front of a compose stack. The listeners, TLS, defaults, and
// observability stay compiled config; routes and upstream pools for
// labeled containers are discovered over the Docker socket and follow
// containers as they start and stop.
//
// TraefikLabels() also honors existing traefik.* labels, so a stack
// already labeled for Traefik migrates without touching its compose files:
//
//	services:
//	  whoami:
//	    image: traefik/whoami
//	    labels:
//	      traefik.enable: "true"
//	      traefik.http.routers.whoami.rule: Host(`whoami.example.com`)
//
// New containers can use the native schema instead:
//
//	services:
//	  api:
//	    image: example/api
//	    labels:
//	      statute.enable: "true"
//	      statute.host: api.example.com
//	      statute.port: "8080"
//	      statute.healthcheck.path: /healthz
package main

import "statute.kjanat.dev"

func main() {
	statute.Main(statute.Config{
		Listeners: statute.Listeners{
			statute.HTTP(":80").RedirectTo("https"),
			statute.HTTPS(":443",
				statute.AutoTLS("example.com", "*.example.com").
					Email("ops@example.com").
					Storage("/var/lib/statute/certs").
					CloudflareDNS01("cf-api-token"),
				statute.HTTP2(),
			),
		},

		// Static routes always win over label-derived ones.
		Routes: statute.Routes{
			statute.Match("/metrics-ui/*").Serve("/srv/dashboard"),
		},

		Docker: statute.Docker().
			Network("proxy"). // take container IPs from this network
			TraefikLabels().  // honor traefik.* labels too
			Refresh("30s"),   // periodic resync on top of the event stream

		Defaults: statute.Defaults{
			ReadHeaderTimeout: "5s",
			WriteTimeout:      "30s",
			IdleTimeout:       "120s",
		},
		Observability: statute.Observability{
			AccessLog: statute.JSONLog(statute.Stdout),
			Metrics:   statute.Prometheus(":9090", "/metrics"),
		},
		Shutdown: statute.Shutdown{GracePeriod: "30s", DrainListeners: true},
	})
}
