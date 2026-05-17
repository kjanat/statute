// dev is a runnable example demonstrating round-robin load balancing across
// three echo backends with active health checks. The companion
// docker-compose.yml spins up the backends; this binary is the statute
// proxy in front of them.
//
// Hit http://localhost:8080/ repeatedly to watch the access log alternate
// between echo-1/2/3. Stop one of the echo containers and watch the
// failover happen (the metrics endpoint at :9090/metrics counts 502 errors
// briefly until the health check demotes the dead backend).
package main

import "github.com/kjanat/statute"

func main() {
	statute.Main(statute.Config{
		Listeners: statute.Listeners{
			statute.HTTP(":8080"),
		},

		Upstreams: statute.Upstreams{
			"echo": statute.Pool{
				Backends: []statute.Backend{
					{Address: "echo-1:5678"},
					{Address: "echo-2:5678"},
					{Address: "echo-3:5678"},
				},
				Strategy: statute.RoundRobin,
				HealthCheck: statute.HealthCheck{
					Path:      "/",
					Interval:  "5s",
					Timeout:   "1s",
					Healthy:   2,
					Unhealthy: 2,
				},
			},
		},

		Routes: statute.Routes{
			statute.Match("/*").ProxyTo("echo").
				With(
					statute.Timeout("10s"),
					statute.RequestID(),
					statute.SecurityHeaders().ContentTypeOptions(true),
				),
		},

		Defaults: statute.Defaults{
			ReadHeaderTimeout: "5s",
			WriteTimeout:      "30s",
			IdleTimeout:       "60s",
		},

		Observability: statute.Observability{
			AccessLog: statute.JSONLog(statute.Stdout),
			Metrics:   statute.Prometheus(":9090", "/metrics"),
		},

		Shutdown: statute.Shutdown{
			GracePeriod:    "5s",
			DrainListeners: true,
		},
	})
}
