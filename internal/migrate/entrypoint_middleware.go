package migrate

import (
	"fmt"
	"strings"

	statute "statute.kjanat.dev"
)

func (g *generator) defaultMiddlewareRefs() ([]string, bool) { return nil, true }

func (g *generator) entrypointMiddleware() {
	for _, name := range keys(g.entrypoints) {
		o := g.entrypoints[name]
		if o.value("http.middlewares") == nil {
			continue
		}
		refs, ok := stringList(o.value("http.middlewares"), false)
		if !ok {
			o.mark("http.middlewares", Manual, "invalid entrypoint middleware list")
			continue
		}
		var values []statute.Middleware
		var code []string
		valid := true
		for _, ref := range refs {
			chain, found := g.chains[ref]
			if !found || !strings.Contains(ref, "@") {
				valid = false
				break
			}
			values = append(values, chain.values...)
			code = append(code, chain.code...)
		}
		if !valid {
			o.mark("http.middlewares", Manual, "entrypoint references a missing or blocked middleware")
			continue
		}
		if len(values) == 0 {
			o.mark("http.middlewares", Exact, "empty entrypoint middleware list")
			continue
		}
		g.entryDefaults.values = append(g.entryDefaults.values, statute.OnListeners([]string{name}, values...))
		g.entryDefaults.code = append(g.entryDefaults.code, fmt.Sprintf("statute.OnListeners([]string{%q},%s)", name, strings.Join(code, ",")))
		o.mark("http.middlewares", Translated, "route-owned middleware selected by listener name")
		for _, ref := range refs {
			g.markMiddlewareUsed(ref)
		}
	}
	if g.r.Config.Docker != nil && len(g.entryDefaults.values) > 0 {
		g.r.Config.Docker.DefaultMiddleware(g.entryDefaults.values...)
		g.docker += ".DefaultMiddleware(" + strings.Join(g.entryDefaults.code, ",") + ")"
	}
}

func (g *generator) middlewareRefs(o *Object) ([]string, bool) { return middlewareRefs(o) }

func (g *generator) checkDefaultMiddleware(o *Object, refs []string) {
	for _, name := range g.routerEntries[o] {
		entry := g.entrypoints[name]
		if entry == nil || entry.value("http.middlewares") == nil {
			continue
		}
		values, ok := stringList(entry.value("http.middlewares"), false)
		if !ok {
			o.note("$entrypoint-policy", Manual, "invalid entrypoint middleware list")
			continue
		}
		for _, ref := range values {
			if _, found := g.chains[ref]; !found {
				o.note("$entrypoint-policy", Manual, "missing or blocked entrypoint middleware")
			}
		}
	}
}
