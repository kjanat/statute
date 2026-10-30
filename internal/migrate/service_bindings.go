package migrate

import (
	"fmt"
	"strings"
)

func (g *generator) discoveredService(name string) bool {
	if !strings.HasSuffix(name, "@docker") || g.r.Config.Docker == nil {
		return false
	}
	object := g.objects["services:"+name]
	return object != nil && object.Generated
}

func (g *generator) bindService(o *Object, name string) bool {
	if g.r.Config.Docker == nil {
		o.note("$provider", Manual, "Docker provider is required for discovered routers")
		return false
	}
	if name == serviceNoop {
		g.r.Config.Docker.BindHandler(name, migrationNoop)
		g.imports["net/http"] = true
		g.docker += fmt.Sprintf(".BindHandler(%q,http.HandlerFunc(func(w http.ResponseWriter,_ *http.Request){w.WriteHeader(http.StatusTeapot)}))", name)
		return true
	}
	if _, ok := g.pools[name]; !ok {
		return false
	}
	g.r.Config.Docker.BindUpstream(name, name)
	g.docker += fmt.Sprintf(".BindUpstream(%q,%q)", name, name)
	return true
}
