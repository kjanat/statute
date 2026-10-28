//go:build e2e

package main

import statute "statute.kjanat.dev"

func requestBufferConfig(string) statute.Config {
	cfg := meshConfig("")
	cfg.RequestBuffers = statute.RequestBufferResources{MemoryBudget: "64KiB", Storage: "/var/lib/statute", DiskBudget: "1MiB"}
	cfg.Upstreams = statute.Upstreams{poolOrigin: statute.Pool{Backends: []statute.Backend{{Address: origin1Addr}}}}
	cfg.Routes = statute.Routes{
		statute.Match("/small/*").ProxyTo(poolOrigin).With(statute.StripPrefix("/small"), statute.RequestBuffer("5MiB")),
		statute.Match("/limited/*").ProxyTo(poolOrigin).With(statute.StripPrefix("/limited"), statute.RequestBuffer("8B").MemoryThreshold("2B")),
		statute.Match("/unavailable/*").ProxyTo(poolOrigin).With(statute.StripPrefix("/unavailable"), statute.RequestBuffer("2MiB").MemoryThreshold("2B")),
	}
	cfg.FallbackRoutes = statute.Routes{statute.Match("/*").ProxyTo(poolOrigin).With(statute.RequestBuffer("8B").MemoryThreshold("2B"))}
	return cfg
}
