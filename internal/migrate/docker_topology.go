package migrate

import (
	"maps"
	"slices"

	"statute.kjanat.dev/internal/docker"
	"statute.kjanat.dev/resolved"
)

func (g *generator) checkDockerExtraction(c *resolved.Config) {
	if c.Docker == nil {
		return
	}
	failed := false
	g.each("routers", func(o *Object) {
		if o.Provider != providerDocker || !o.Generated {
			return
		}
		networks := map[string]string{"migration": "192.0.2.1"}
		for _, name := range []string{c.Docker.Network, o.labels["traefik.docker.network"]} {
			if name != "" {
				networks[name] = "192.0.2.1"
			}
		}
		labels := maps.Clone(o.labels)
		labels["com.docker.compose.service"] = o.containerName
		container := docker.Container{ID: o.Source, Name: o.containerName, Labels: labels, Ports: o.exposedPorts, Networks: networks, Running: true}
		bound := map[string]bool{}
		for name := range c.Docker.ServiceBindings {
			bound[name] = true
		}
		services, _, _ := docker.Extract(container, docker.ExtractOptions{TraefikLabels: true, BoundServices: bound, ExposedByDefault: c.Docker.ExposedByDefault, Network: c.Docker.Network})
		matchers, err := docker.ParseRule(o.text("rule"))
		if err != nil {
			failed = true
			return
		}
		refs, _ := middlewareRefs(o)
		for i := range matchers {
			matchers[i].Middlewares = refs
			matchers[i].Priority = priority(o)
			if entries, ok := middlewareRefsForEntries(o); ok {
				matchers[i].ListenerNames = entries
			}
		}
		if !extractedRouteMatches(services, o.text("service")+"@traefik", matchers) {
			o.note("$label-runtime", Manual, "retained labels do not reproduce the audited service/matcher/middleware edges in the shared Docker parser")
			failed = true
		}
	})
	code := "retained-labels-match-shared-docker-parser"
	if failed {
		code = "retained-label-topology-mismatch"
	}
	g.r.Report.Checks = append(g.r.Report.Checks, Check{Tool: toolTopology, Code: code, Blocking: failed})
}

func extractedRouteMatches(services []docker.Service, name string, want []docker.Matcher) bool {
	for _, service := range services {
		if service.Name != name {
			continue
		}
		for _, matcher := range want {
			if !slices.ContainsFunc(service.Routes, func(got docker.Matcher) bool { return got.Equal(matcher) }) {
				return false
			}
		}
		return true
	}
	return false
}

func middlewareRefsForEntries(o *Object) ([]string, bool) {
	if o.value("entryPoints") == nil {
		return nil, true
	}
	return stringList(o.value("entryPoints"), o.Provider == providerDocker)
}
