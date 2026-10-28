package migrate

import (
	"fmt"
	"strings"
)

func (g *generator) listenerDefaults() string {
	var parts []string
	for _, f := range []struct {
		key, field, fallback string
		dst                  *string
	}{
		{"readTimeout", "ReadTimeout", "60s", &g.r.Config.Defaults.ReadTimeout},
		{"writeTimeout", "WriteTimeout", "0s", &g.r.Config.Defaults.WriteTimeout},
		{"idleTimeout", "IdleTimeout", "180s", &g.r.Config.Defaults.IdleTimeout},
	} {
		path := "transport.respondingTimeouts." + f.key
		value, ok := g.commonListenerValue(path, f.fallback)
		if !ok {
			continue
		}
		*f.dst = value
		parts = append(parts, fmt.Sprintf("%s:%q", f.field, value))
		if f.key == "readTimeout" {
			g.r.Config.Defaults.ReadHeaderTimeout = value
			parts = append(parts, fmt.Sprintf("ReadHeaderTimeout:%q", value))
		}
	}
	// Explicit max-header sizes must agree because the native defaults are global.
	if value, ok := g.commonListenerValue("http.maxHeaderBytes", "1048576"); ok {
		if n, err := integer(value); err == nil && n > 0 {
			g.r.Config.Defaults.MaxHeaderBytes = n
			parts = append(parts, fmt.Sprintf("MaxHeaderBytes:%d", n))
		}
	}
	return "statute.Defaults{" + strings.Join(parts, ",") + "}"
}

func (g *generator) commonListenerValue(path, fallback string) (string, bool) {
	value := ""
	valid := true
	for _, name := range keys(g.entrypoints) {
		o := g.entrypoints[name]
		v := scalar(o.value(path))
		if v == "" {
			v = fallback
		}
		if !literal(v) {
			valid = false
		}
		if value != "" && value != v {
			valid = false
		}
		value = v
	}
	if !valid {
		for _, name := range keys(g.entrypoints) {
			g.entrypoints[name].mark(path, Manual, "per-entrypoint values differ or are unresolved; native server defaults apply to every listener")
		}
		return "", false
	}
	for _, name := range keys(g.entrypoints) {
		g.entrypoints[name].mark(path, Translated, "same effective value on all listeners; emitted once in server Defaults")
	}
	if value == "" {
		value = fallback
	}
	return value, true
}
