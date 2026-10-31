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
	g.redirectMiddleware(o, &chain)
	g.securityHeaders(o, &chain)
	g.headers(o, &chain)
	g.rateLimit(o, &chain)
	g.basicAuth(o, &chain)
	g.nativeRewrite(o, &chain)
	g.buffering(o, &chain)
	g.retry(o, &chain)
	g.compression(o, &chain)
	g.ipAllowList(o, &chain)
	g.expandChain(o, &chain)
	classifyEmptyHeaders(o, chain)
	if len(chain.values) > 0 && o.value("chain.middlewares") == nil {
		chain.values = []statute.Middleware{statute.InOrder(chain.values...)}
		chain.code = []string{"statute.InOrder(" + strings.Join(chain.code, ",") + ")"}
	}
	if nativeMiddlewareReady(o, chain) {
		registered := middleware{values: chain.values}
		if len(chain.values) > 0 {
			registered.parts = []middlewarePart{{call: g.reuse(middlewareType, id, o.identity(), sliceExpr(chain.segments()))}}
		}
		g.chains[id] = registered
		return registered, true
	}
	g.retainMiddlewareCandidate(o, chain)
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
			g.docker += fmt.Sprintf(".Middleware(%q%s)", ref, middlewareArguments(mw.parts))
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
	selection, code, ok := sourceIPSelection(o, "ipAllowList.ipStrategy")
	if !ok {
		return
	}
	chain.values = append(chain.values, statute.AllowIPs(cidrs...).SelectIP(selection))
	chain.code = append(chain.code, "statute.AllowIPs("+quoted(cidrs)+").SelectIP("+code+")")
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
			o.mark(path, Exact, "provider-qualified chain dependencies are reported separately")
			o.note("$middleware:"+qualified(ref, o.Provider), Manual, "chain blocked by referenced middleware; see its field findings")
			ok = false
			continue
		}
		if len(chain.values)+len(child.values) > 256 {
			o.note("$size", Unsupported, "expanded middleware chain exceeds 256 operations")
			return
		}
		chain.values = append(chain.values, child.values...)
		chain.parts = append(chain.parts, child.parts...)
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
			o.mark(f.Path, OutOfScope, "plugins require compiled Go behavior outside HTTP migration")
		case "basicauth", "forwardauth", "digestauth":
			o.mark(f.Path, Manual, "security policy requires explicit compiled registration and secret references; credentials are withheld")
		}
	}
}

func middlewareArguments(parts []middlewarePart) string {
	if len(parts) == 0 {
		return ""
	}
	return "," + variadicArgs(parts)
}

func classifyEmptyHeaders(o *Object, chain middleware) {
	if len(chain.values) == 0 && o.value("headers.stsSeconds") == nil && (o.value("headers") != nil || hasFieldPrefix(o, "headers.")) && o.ready() {
		o.note("$empty-header-policy", Manual, "source header middleware with every feature disabled is invalid; remove it or select an active header policy")
	}
}

func nativeMiddlewareReady(o *Object, chain middleware) bool {
	return o.readyWithBindings() && (len(chain.values) > 0 || o.value("headers.stsSeconds") != nil || o.value("chain.middlewares") != nil)
}
