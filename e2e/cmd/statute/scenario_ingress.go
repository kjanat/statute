//go:build e2e

package main

import statute "statute.kjanat.dev"

func ingressConfig(string) statute.Config {
	return statute.Config{
		Listeners: statute.Listeners{statute.HTTP(":8080").With(statute.IngressPolicy{
			ForwardedHeaders: true, NormalizePath: true, SanitizePath: true,
			HeaderAliases: statute.HeaderAliasesReject,
		})},
		Upstreams: statute.Upstreams{poolOrigin: statute.Pool{Backends: []statute.Backend{{Address: origin1Addr}}}},
		Routes: statute.Routes{
			statute.Match("/forwarded").ProxyTo(poolOrigin).With(statute.AddRequestHeader("X-Forwarded-For", "198.51.100.7")),
			statute.Match("/upload/*").ProxyTo(poolOrigin).With(statute.StripPrefix("/upload"), statute.Retry(2, statute.OnStatus(502))),
			statute.Match("/api/*").ProxyTo(poolOrigin).With(statute.StripPrefix("/api")),
			statute.Match("/private").ProxyTo(poolOrigin).With(statute.AllowIPs("127.0.0.0/8")),
			statute.Match("/limited").ProxyTo(poolOrigin).With(statute.RateLimit("0.5/s")),
		},
		Defaults: e2eDefaults(), Observability: e2eObservability(), Shutdown: e2eShutdown(),
	}
}
