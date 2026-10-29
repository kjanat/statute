package migrate

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"statute.kjanat.dev/internal/docker"
)

func unsupportedRuleReason(rule string) string {
	if strings.Contains(rule, "ClientIP(") {
		return "ClientIP needs compiled ClientIPs route policy; the shared rule parser cannot translate this matcher or choose its proxy-trust semantics"
	}
	for _, name := range []string{"HostRegexp", "QueryRegexp", "Query", "HeaderRegexp", "Header", "Method"} {
		if strings.Contains(rule, name+"(") {
			return name + " matcher has no equivalent native route declaration"
		}
	}
	return "unsupported or unresolved matcher grammar; source rule withheld"
}

func (g *generator) inferDockerService(o *Object) bool {
	if o.text("service") != "" {
		return true
	}
	var candidate *Object
	for _, service := range g.r.Report.Objects {
		if service.Kind != "services" || service.Provider != providerDocker || service.Source != o.Source {
			continue
		}
		if candidate != nil {
			o.note("$service-selection", Manual, "multiple exposed services require an explicit router service reference")
			return false
		}
		candidate = service
	}
	if candidate == nil {
		o.note("$service-selection", Manual, "no declared service supplies an audited port; provide an explicit exposed service or service/port label")
		return false
	}
	o.data["service"] = candidate.Name
	o.note("$service-selection", Translated, "selected the container's sole declared service")
	return true
}

func (g *generator) crossProviderRoute(o *Object) {
	ms, err := docker.ParseRule(o.text("rule"))
	if err != nil || !nativePaths(ms) {
		return
	}
	name := fmt.Sprintf("migrationCrossProviderRoute%d", len(g.helpers))
	refs, _ := g.middlewareRefs(o)
	var middleware []string
	for _, ref := range refs {
		chain, ok := g.chains[qualified(ref, o.Provider)]
		if !ok {
			g.helpers[name] = fmt.Sprintf("func %s() statute.Routes {panic(%q)}", name, "resolve cross-provider route middleware before selecting a compiled route")
			return
		}
		middleware = append(middleware, chain.code...)
	}
	var routes []string
	for _, m := range ms {
		code := fmt.Sprintf("statute.Match(%q).ProxyTo(%q)", m.Path, o.text("service"))
		if hosts := hostSpellings(m.Host); len(hosts) > 0 {
			code += ".Hosts(" + quoted(hosts) + ")"
		}
		if len(middleware) > 0 {
			code += ".With(" + strings.Join(middleware, ",") + ")"
		}
		routes = append(routes, code)
	}
	g.helpers[name] = fmt.Sprintf("// %s requires explicit route order and middleware; see the audit.\nfunc %s() statute.Routes {return statute.Routes{%s}}", name, name, strings.Join(routes, ","))
}

// Only explicit Compose target ports are evidence; image metadata is never queried.
func composeExposedPorts(service map[string]any) []int {
	var ports []int
	for _, key := range []string{"expose", "ports"} {
		items, _ := service[key].([]any)
		for _, item := range items {
			n, valid := composeTargetPort(item)
			if !valid {
				return nil
			}
			if n == 0 {
				continue
			}

			if !slices.Contains(ports, n) {
				ports = append(ports, n)
			}
		}
	}
	slices.Sort(ports)
	return ports
}

func inventoryImplicitDockerService(r *Report, source, name string, service map[string]any, objects map[string]map[string]any, labels map[string]string, invalid bool) {
	if invalid {
		return
	}
	router := false
	for id := range objects {
		kind, _, _ := strings.Cut(id, ":")
		if kind == "services" {
			return
		}
		router = router || kind == "routers"
	}
	ports := composeExposedPorts(service)
	if !router || len(ports) != 1 {
		return
	}
	o := addObject(r, source, providerDocker, "services", name, map[string]any{dockerServerPort: strconv.Itoa(ports[0])})
	o.labels, o.containerName, o.exposedPorts = labels, name, ports
	o.note("$exposed-port", Translated, "container's sole explicit Compose TCP target port supplies the implicit native service; image ports are not inspected")
}

func composeTargetPort(item any) (int, bool) {
	port := scalar(item)
	if m, ok := item.(map[string]any); ok {
		port = scalar(m["target"])
		if protocol := scalar(m["protocol"]); protocol != "" && protocol != protocolTCP {
			return 0, true
		}
	}
	value, protocol, _ := strings.Cut(port, "/")
	if protocol != "" && protocol != protocolTCP {
		return 0, true
	}
	if i := strings.LastIndexByte(value, ':'); i >= 0 {
		value = value[i+1:]
	}
	n, err := strconv.Atoi(value)
	return n, err == nil && n > 0 && n <= 65535
}

func (g *generator) accountBlockedRouter(o *Object) {
	g.classifyRouterPolicy(o)
	if refs, ok := g.middlewareRefs(o); ok {
		g.routerMiddleware(o, refs)
	}
	if name := o.text("service"); literal(name) && name != "" {
		o.reference("service", "service", name)
		if g.objects["services:"+qualified(name, o.Provider)] != nil {
			o.mark("service", Exact, "service reference inventoried; unsupported matcher prevents route generation")
		} else {
			o.mark("service", Manual, "referenced service is absent from supplied inputs")
		}
	}
}
