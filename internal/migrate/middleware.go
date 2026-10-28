package migrate

import (
	"fmt"
	"net/netip"
	"strings"

	statute "statute.kjanat.dev"
)

func (g *generator) chain(o *Object) (middleware, bool) {
	id := o.Name + "@" + o.Provider
	if chain, ok := g.chains[id]; ok {
		return chain, true
	}
	if g.visiting[id] {
		o.note("$cycle", Manual, "middleware reference cycle")
		return middleware{}, false
	}
	if g.attempted[id] {
		return middleware{}, false
	}
	g.attempted[id] = true
	if len(g.visiting) >= 64 {
		o.note("$depth", Unsupported, "middleware chain nesting exceeds 64")
		return middleware{}, false
	}
	g.visiting[id] = true
	defer delete(g.visiting, id)
	checkMiddlewareShape(o)
	classifyMiddlewareDifferences(o)
	var chain middleware
	g.headerRemovals(o, &chain)
	g.ipAllowList(o, &chain)
	g.expandChain(o, &chain)
	if o.ready() && len(chain.values) > 0 {
		g.chains[id] = chain
		return chain, true
	}
	return middleware{}, false
}

func stringList(v any, comma bool) ([]string, bool) {
	var out []string
	if comma {
		s, ok := v.(string)
		if !ok {
			return nil, false
		}
		if strings.Count(s, ",") >= 256 {
			return nil, false
		}
		for part := range strings.SplitSeq(s, ",") {
			out = append(out, strings.TrimSpace(part))
		}
	} else {
		a, ok := v.([]any)
		if !ok {
			return nil, false
		}
		if len(a) > 256 {
			return nil, false
		}
		for _, item := range a {
			s, ok := item.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
	}
	return out, validStrings(out)
}

func validStrings(values []string) bool {
	for _, v := range values {
		if v == "" || !literal(v) {
			return false
		}
	}
	return true
}

func (g *generator) registerMiddleware(routers []*Object) {
	registered := map[string]bool{}
	for _, o := range routers {
		if o.Provider != providerDocker {
			continue
		}
		refs, ok := middlewareRefs(o)
		if !ok {
			continue
		}
		for _, ref := range refs {
			mw, found := g.chains[qualified(ref, providerDocker)]
			if !found || registered[ref] {
				continue
			}
			registered[ref] = true
			g.r.Config.Docker.Middleware(ref, mw.values...)
			g.docker += fmt.Sprintf(".Middleware(%q,%s)", ref, strings.Join(mw.code, ","))
			g.markMiddlewareUsed(qualified(ref, providerDocker))
		}
	}
}

func checkMiddlewareShape(o *Object) {
	families := map[string]bool{}
	for key := range o.data {
		family, _, _ := strings.Cut(key, ".")
		families[strings.ToLower(family)] = true
	}
	if len(families) != 1 {
		o.note("$shape", Manual, "a Traefik middleware must define exactly one middleware type")
	}
}

func (g *generator) markMiddlewareUsed(id string) {
	o := g.objects["middlewares:"+id]
	if o == nil || o.Generated {
		return
	}
	o.Generated = true
	refs, _ := stringList(o.value("chain.middlewares"), o.Provider == providerDocker)
	for _, ref := range refs {
		g.markMiddlewareUsed(qualified(ref, o.Provider))
	}
}

func (g *generator) markMiddlewaresUsed(refs []string, provider string) {
	for _, ref := range refs {
		g.markMiddlewareUsed(qualified(ref, provider))
	}
}

func (g *generator) headerRemovals(o *Object, chain *middleware) {
	path := "headers.customResponseHeaders"
	if o.Provider == providerDocker {
		path = "headers.customresponseheaders"
	}
	for _, f := range o.Fields {
		if !strings.HasPrefix(f.Path, path+".") {
			continue
		}
		value, ok := o.value(f.Path).(string)
		if !ok || value != "" {
			continue
		}
		name := strings.TrimPrefix(f.Path, path+".")
		if !literal(name) {
			continue
		}
		if len(chain.values) >= 256 {
			o.note("$size", Unsupported, "expanded middleware chain exceeds 256 operations")
			return
		}
		chain.values = append(chain.values, statute.RemoveResponseHeader(name))
		chain.code = append(chain.code, fmt.Sprintf("statute.RemoveResponseHeader(%q)", name))
		o.mark(f.Path, Translated, "code-owned response-header removal; header values are never copied")
	}
}

func (g *generator) ipAllowList(o *Object, chain *middleware) {
	path := "ipAllowList.sourceRange"
	if o.Provider == providerDocker {
		path = "ipallowlist.sourcerange"
	}
	cidrs, ok := stringList(o.value(path), o.Provider == providerDocker)
	if !ok || len(cidrs) == 0 {
		return
	}
	for _, cidr := range cidrs {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			return
		}
	}
	chain.values = append(chain.values, statute.AllowIPs(cidrs...))
	chain.code = append(chain.code, "statute.AllowIPs("+quoted(cidrs)+")")
	o.mark(path, Translated, "route-owned client-IP allow list; listener trusted-proxy policy remains explicit")
}

func (g *generator) expandChain(o *Object, chain *middleware) {
	path := "chain.middlewares"
	refs, ok := stringList(o.value(path), o.Provider == providerDocker)
	if !ok || len(refs) == 0 {
		return
	}
	for _, ref := range refs {
		o.reference("chain.middlewares", "middleware", ref)
		dep := g.objects["middlewares:"+qualified(ref, o.Provider)]
		if dep == nil {
			ok = false
			continue
		}
		child, valid := g.chain(dep)
		if !valid {
			ok = false
			continue
		}
		if len(chain.values)+len(child.values) > 256 {
			o.note("$size", Unsupported, "expanded middleware chain exceeds 256 operations")
			return
		}
		chain.values = append(chain.values, child.values...)
		chain.code = append(chain.code, child.code...)
	}
	if ok {
		o.mark(path, Translated, "provider-qualified middleware chain expanded in declaration order")
	}
}

func classifyMiddlewareDifferences(o *Object) {
	for _, f := range o.Fields {
		family, _, _ := strings.Cut(strings.ToLower(f.Path), ".")
		switch family {
		case "buffering":
			o.mark(f.Path, Manual, "buffering affects request admission, response buffering and streaming; explicitly select Statute RequestBuffer resources and route policy")
		case "retry":
			o.mark(f.Path, Manual, "retry attempts, idempotency and request/response buffering differ; review before mapping")
		case "stripprefix", "addprefix", "replacepath", "replacepathregex":
			o.mark(f.Path, Manual, "native rewrites are hoisted once at route entry; verify headers, middleware order and Retry re-entry")
		case "plugin":
			o.mark(f.Path, Unsupported, "Traefik plugins cannot become runtime configuration; supply compiled Go behavior")
		case "basicauth", "forwardauth", "digestauth":
			o.mark(f.Path, Manual, "security policy requires explicit compiled registration and secret references; credentials are withheld")
		}
	}
}
