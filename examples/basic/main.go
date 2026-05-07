package main

import "github.com/kjanat/statute"

func main() {
	statute.Run(statute.Config{
		Listeners: statute.Listeners{
			statute.HTTP(":80").RedirectTo("https"),
			statute.HTTPS(":443",
				statute.AutoTLS("example.com", "api.example.com").
					Email("ops@example.com").
					Storage("/var/lib/statute/certs"),
				statute.HTTP2(),
				statute.HTTP3(":443/udp"),
			),
		},

		Upstreams: statute.Upstreams{
			"api": statute.Pool{
				Backends: []statute.Backend{
					{Address: "10.0.0.1:8080", Weight: 2},
					{Address: "10.0.0.2:8080", Weight: 1},
					{Address: "10.0.0.3:8080", Backup: true},
				},
				Strategy: statute.LeastConnections,
				HealthCheck: statute.HealthCheck{
					Path:     "/healthz",
					Interval: "10s",
					Timeout:  "2s",
				},
				Transport: statute.Transport{
					MaxIdleConnsPerHost: 10,
					IdleConnTimeout:     "90s",
				},
			},
		},

		Routes: statute.Routes{
			statute.Match("/api/v1/*").Host("api.example.com").ProxyTo("api").
				With(
					statute.RateLimit("100/min").Per(statute.ClientIP),
					statute.Retry(3, statute.OnStatus(502, 503, 504)),
					statute.Timeout("30s"),
				),

			statute.Match("/static/*").Serve("./public").
				With(
					statute.Cache("1h"),
					statute.Compress(statute.Gzip, statute.Brotli),
					statute.ETag(),
				),

			// catch-all — keep last; statute matches in declaration order
			statute.Match("/*").Host("example.com").ProxyTo("api").
				With(statute.Timeout("30s")),
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
