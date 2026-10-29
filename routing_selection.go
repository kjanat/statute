package statute

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	"statute.kjanat.dev/internal/docker"
	"statute.kjanat.dev/resolved"
)

type listenerNameKey struct{}

func namedListener(name string, next http.Handler) http.Handler {
	if name == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), listenerNameKey{}, name)))
	})
}

func validateListenerNames(cfg *resolved.Config) error {
	names := map[string]bool{}
	for _, l := range cfg.Listeners {
		if l.Name == "" {
			continue
		}
		if names[l.Name] {
			return fmt.Errorf("duplicate listener name %q", l.Name)
		}
		names[l.Name] = true
	}
	for _, routes := range [][]*resolved.Route{cfg.Routes, cfg.FallbackRoutes} {
		for _, route := range routes {
			var checkMiddlewareNames func([]resolved.Middleware) error
			checkMiddlewareNames = func(chain []resolved.Middleware) error {
				for _, m := range chain {
					for _, name := range m.MiddlewareListeners {
						if !names[name] {
							return fmt.Errorf("middleware references unknown listener %q", name)
						}
					}
					if err := checkMiddlewareNames(m.Group); err != nil {
						return err
					}
				}
				return nil
			}
			if err := checkMiddlewareNames(route.Middleware); err != nil {
				return err
			}
			for _, name := range route.Listeners {
				if !names[name] {
					return fmt.Errorf("route references unknown listener %q", name)
				}
			}
		}
	}
	return nil
}

func compileRouteMatcher(r *resolved.Route, prioritized bool) docker.Matcher {
	m := docker.CompileNative(r.Host, r.Pattern)
	switch r.PathMatch {
	case "exact":
		m.PathKind = docker.PathExact
	case "byte_prefix":
		m.PathKind = docker.PathByte
	}
	m.Priority, m.PriorityOrder = r.Priority, prioritized
	m.ListenerNames = append([]string(nil), r.Listeners...)
	return m
}

func bestPriorityRoute(routes []compiledRoute, host string, r *http.Request) *compiledRoute {
	var best *compiledRoute
	for i := range routes {
		candidate := findCompiledRoute(routes[i:i+1], host, r)
		if candidate != nil && (best == nil || candidate.matcher.Priority > best.matcher.Priority) {
			best = candidate
		}
	}
	return best
}

func priorityDynamicRoute(t *dynamicTable, host string, r *http.Request) *compiledRoute {
	serving := bestPriorityRoute(t.routes, host, r)
	quarantine := bestPriorityRoute(t.quarantines, host, r)
	rejection := bestPriorityRoute(t.rejections, host, r)
	if quarantine != nil && (serving == nil || quarantine.matcher.Priority >= serving.matcher.Priority) {
		return quarantine
	}
	if rejection != nil && (serving == nil || rejection.matcher.Priority >= serving.matcher.Priority) {
		return rejection
	}
	if serving != nil {
		return serving
	}
	return findCompiledRoute(t.tombstones, host, r)
}

func listenerMatches(names []string, r *http.Request) bool {
	if len(names) == 0 {
		return true
	}
	name, _ := r.Context().Value(listenerNameKey{}).(string)
	return slices.Contains(names, name)
}
