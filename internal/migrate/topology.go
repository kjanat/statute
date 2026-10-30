package migrate

import (
	"reflect"
	"slices"
	"strings"

	statute "statute.kjanat.dev"
	"statute.kjanat.dev/internal/docker"
	"statute.kjanat.dev/resolved"
)

// Check source-derived edges so route permutations and policies attached
// to the wrong shared pool fail even when route and pool counts match.
func (g *generator) checkSourceTopology(c *resolved.Config) {
	routes, fallbacks := slices.Clone(c.Routes[:len(c.Routes)-len(g.internalRoutes)]), slices.Clone(c.FallbackRoutes)
	failed := false
	for _, o := range g.orderedRouters() {
		if !g.activeFileRouter(o) {
			continue
		}
		ms, err := docker.ParseRule(o.text("rule"))
		if err != nil {
			failed = true
			continue
		}
		for _, m := range ms {
			target := &routes
			if g.terminalMatcher(m) {
				target = &fallbacks
			}
			failed = g.checkSourceMatcher(c, o, m, target) || failed
		}
	}
	if len(routes)+len(fallbacks) != 0 {
		failed = true
	}
	code := "source-route-order-matcher-action-policy-edges-match"
	if failed {
		code = "source-route-topology-mismatch"
	}
	g.r.Report.Checks = append(g.r.Report.Checks, Check{Tool: toolTopology, Code: code, Blocking: failed})
}

func (g *generator) checkSourceMatcher(c *resolved.Config, o *Object, m docker.Matcher, routes *[]*resolved.Route) bool {
	hosts := hostSpellings(m.Host)
	if len(hosts) == 0 {
		hosts = []string{""}
	}
	if len(*routes) < len(hosts) {
		return true
	}
	failed := false
	for _, host := range hosts {
		r := (*routes)[0]
		*routes = (*routes)[1:]
		failed = failed || r.Host != host || r.Pattern != m.Path || !slices.Equal(r.PeerIPCIDRs, m.PeerIPCIDRs) || r.Priority != priority(o) || !slices.Equal(r.Listeners, g.routerEntries[o])

		failed = g.checkRoutePolicy(c, o, r) || failed
	}
	return failed
}

func (g *generator) checkDockerTopology(c *resolved.Config) {
	if c.Docker == nil {
		return
	}
	failed := false
	g.each("routers", func(o *Object) {
		if !o.Generated || o.Provider != providerDocker {
			return
		}
		refs, _ := g.middlewareRefs(o)
		for _, ref := range refs {
			want := g.chains[qualified(ref, providerDocker)]
			plan := statute.Config{Listeners: g.r.Config.Listeners, Docker: statute.Docker().TraefikLabels().Middleware(ref, want.values...)}
			expected, err := statute.Resolve(plan)
			if err != nil || !reflect.DeepEqual(c.Docker.Middleware[ref], expected.Docker.Middleware[ref]) {
				failed = true
			}
		}
		if !dockerServiceHasPolicy(c.Docker, o.text("service")) {
			failed = true
		}
	})
	code := "docker-router-registry-and-exact-service-policy-edges-match"
	if failed {
		code = "docker-policy-topology-mismatch"
	}
	g.r.Report.Checks = append(g.r.Report.Checks, Check{Tool: toolTopology, Code: code, Blocking: failed})
}

func (g *generator) bindingChecks() {
	deferred := false
	for _, check := range g.r.Report.Checks {
		deferred = deferred || check.Deferred
	}
	if !deferred {
		return
	}
	for i := range g.r.Report.Checks {
		c := &g.r.Report.Checks[i]
		if slices.Contains([]string{toolResolve, toolLint, toolExport, toolGraph}, c.Tool) {
			c.Deferred = true
		}
	}
}

func (g *generator) checkRoutePolicy(c *resolved.Config, o *Object, r *resolved.Route) bool {
	refs, _ := g.middlewareRefs(o)
	if r.Redirect != nil {
		return true
	}
	chain := append([]statute.Middleware(nil), g.entryDefaults.values...)
	for _, ref := range refs {
		chain = append(chain, g.chains[qualified(ref, o.Provider)].values...)
	}
	name := qualified(o.text("service"), o.Provider)
	plan := g.r.Config
	route := statute.Match("/*").ProxyTo(name).With(chain...)
	if name == serviceNoop {
		route = statute.Match("/*").Handle(migrationNoop).With(chain...)
	} else if g.discoveredService(name) {
		route = statute.Match("/*").ProxyToDiscovered(strings.TrimSuffix(name, "@docker") + "@traefik").With(chain...)
	}
	plan.Routes = statute.Routes{route}
	plan.FallbackRoutes = nil
	expected, err := statute.Resolve(plan)
	if err != nil || len(expected.Routes) != 1 {
		return true
	}
	return !reflect.DeepEqual(r.Middleware, expected.Routes[0].Middleware) || r.Upstream != c.Upstreams[name] || r.HandlerRoute != expected.Routes[0].HandlerRoute || r.DiscoveredUpstream != expected.Routes[0].DiscoveredUpstream
}

func generatedFileRouter(o *Object) bool { return o.Generated && o.Provider == providerFile }

func (g *generator) activeFileRouter(o *Object) bool {
	return generatedFileRouter(o) && !g.disabledRouters[o]
}

func dockerServiceHasPolicy(d *resolved.Docker, name string) bool {
	_, ok := d.PoolPolicy[name+"@traefik"]
	binding := d.ServiceBindings[name]
	return ok || binding.Upstream != "" || binding.HandlerBinding
}
