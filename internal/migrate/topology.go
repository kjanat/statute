package migrate

import (
	"reflect"
	"slices"
	"strconv"

	statute "statute.kjanat.dev"
	"statute.kjanat.dev/internal/docker"
	"statute.kjanat.dev/resolved"
)

// Check source-derived edges so route permutations and policies attached
// to the wrong shared pool fail even when route and pool counts match.
func (g *generator) checkSourceTopology(c *resolved.Config) {
	routes, fallbacks := slices.Clone(c.Routes), slices.Clone(c.FallbackRoutes)
	failed := false
	for _, o := range g.orderedRouters() {
		if !generatedFileRouter(o) {
			continue
		}
		ms, err := docker.ParseRule(o.text("rule"))
		if err != nil {
			failed = true
			continue
		}
		for _, m := range ms {
			target := &routes
			if m.Host == "" && m.PathKind == docker.PathAny {
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
		failed = failed || r.Host != host || r.Pattern != m.Path

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
		if _, ok := c.Docker.PoolPolicy[o.text("service")+"@traefik"]; !ok {
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
	if redirect := g.expectedRedirect(o, refs); redirect != nil {
		return !reflect.DeepEqual(r.Redirect, redirect)
	}
	if r.Redirect != nil {
		return true
	}
	var chain []statute.Middleware
	for _, ref := range refs {
		chain = append(chain, g.chains[qualified(ref, o.Provider)].values...)
	}
	name := qualified(o.text("service"), o.Provider)
	plan := g.r.Config
	plan.Routes = statute.Routes{statute.Match("/*").ProxyTo(name).With(chain...)}
	plan.FallbackRoutes = nil
	expected, err := statute.Resolve(plan)
	if err != nil || len(expected.Routes) != 1 {
		return true
	}
	return !reflect.DeepEqual(r.Middleware, expected.Routes[0].Middleware) || r.Upstream != c.Upstreams[name]
}

func (g *generator) expectedRedirect(o *Object, refs []string) *resolved.Redirect {
	if len(refs) != 1 {
		return nil
	}
	mw := g.objects["middlewares:"+qualified(refs[0], o.Provider)]
	if mw == nil {
		return nil
	}
	scheme := mw.text("redirectScheme.scheme")
	if scheme != protocolHTTP && scheme != protocolHTTPS {
		return nil
	}
	status := 302
	permanent, _ := strconv.ParseBool(scalar(mw.value("redirectScheme.permanent")))
	if permanent {
		status = 301
	}
	return &resolved.Redirect{Target: scheme + "://{host}{request_uri}", Status: status}
}

func generatedFileRouter(o *Object) bool { return o.Generated && o.Provider == providerFile }
