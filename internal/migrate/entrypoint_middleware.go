package migrate

import (
	"fmt"
	"slices"
	"strings"
)

func (g *generator) defaultMiddlewareRefs() ([]string, bool) {
	var common []string
	for i, name := range g.contentEntries {
		o := g.entrypoints[name]
		refs := []string(nil)
		if o.value("http.middlewares") != nil {
			var ok bool
			refs, ok = stringList(o.value("http.middlewares"), false)
			if !ok {
				return nil, false
			}
		}
		// Static entrypoint middleware names must include a provider suffix.
		for _, ref := range refs {
			if !strings.Contains(ref, "@") {
				return nil, false
			}
		}
		if i == 0 {
			common = refs
		} else if !slices.Equal(common, refs) {
			return nil, false
		}
	}
	return common, true
}

func (g *generator) entrypointMiddleware() {
	refs, ok := g.defaultMiddlewareRefs()
	if !ok {
		return
	}
	var chain middleware
	for _, ref := range refs {
		mw, found := g.chains[ref]
		if !found {
			return
		}
		chain.values = append(chain.values, mw.values...)
		chain.code = append(chain.code, mw.code...)
	}
	if len(chain.values) > 256 {
		return
	}
	for _, name := range g.contentEntries {
		g.entrypoints[name].mark("http.middlewares", Translated, "common content-listener middleware prepended to every route; Docker uses compiled DefaultMiddleware")
	}
	if g.r.Config.Docker != nil && len(chain.values) > 0 {
		g.r.Config.Docker.DefaultMiddleware(chain.values...)
		g.docker += ".DefaultMiddleware(" + strings.Join(chain.code, ",") + ")"
		for _, ref := range refs {
			g.markMiddlewareUsed(ref)
		}
	}
}

func (g *generator) middlewareRefs(o *Object) ([]string, bool) {
	refs, ok := middlewareRefs(o)
	if !ok {
		return nil, false
	}
	defaults, valid := g.defaultMiddlewareRefs()
	if !valid {
		o.note("$entrypoint-middleware", Manual, "entrypoint middleware differs across shared content listeners or lacks provider-qualified identity")
		return refs, true
	}
	if o.Provider == providerDocker {
		return refs, true
	}
	return append(append([]string(nil), defaults...), refs...), true
}

func (g *generator) checkDefaultMiddleware(o *Object, refs []string) {
	defaults, ok := g.defaultMiddlewareRefs()
	if !ok {
		return
	}
	if len(defaults)+len(refs) > 256 {
		o.note("$size", Unsupported, "entrypoint and router middleware exceed 256 references")
		return
	}
	for _, ref := range defaults {
		if _, exists := g.chains[ref]; !exists {
			o.note("$entrypoint-policy", Manual, fmt.Sprintf("missing or blocked entrypoint middleware %q", ref))
		}
	}
}
