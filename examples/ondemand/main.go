// The ondemand example keeps a Docker route available while its container is
// stopped. Compose supplies the persistent registry and Docker socket; labels
// select the service, but this compiled policy alone grants lifecycle authority.
package main

import statute "statute.kjanat.dev"

func main() {
	const shortWindow = "10s"
	statute.Main(statute.Config{
		Listeners: statute.Listeners{statute.HTTP(":8080")},
		Docker: statute.Docker().Storage("/var/lib/statute/docker").
			Workload("ondemand-demo", statute.WorkloadPolicy{
				IdleAfter: shortWindow, StartTimeout: shortWindow, ReadyTimeout: "20s",
				Readiness: statute.HTTPReadiness("/health"),
			}),
		Observability: statute.Observability{
			AccessLog: statute.JSONLog(statute.Stdout),
			Metrics:   statute.Prometheus(":9090", "/metrics"),
			Health:    statute.Health(":8081", "/healthz"),
		},
		Shutdown: statute.Shutdown{GracePeriod: shortWindow, DrainListeners: true},
	})
}
