package statute

import (
	"fmt"
	"net/http"

	"statute.kjanat.dev/internal/docker"
	"statute.kjanat.dev/resolved"
)

func resolveServiceBindings(bindings map[string]ServiceBinding) map[string]resolved.ServiceBinding {
	if len(bindings) == 0 {
		return nil
	}
	out := make(map[string]resolved.ServiceBinding, len(bindings))
	for name, binding := range bindings {
		out[name] = resolved.ServiceBinding{Upstream: binding.Upstream, Handler: binding.Handler, HandlerBinding: binding.Handler != nil}
	}
	return out
}

func validateServiceBindings(cfg *resolved.Config) error {
	if err := validateDockerServiceBindings(cfg); err != nil {
		return err
	}
	for _, routes := range [][]*resolved.Route{cfg.Routes, cfg.FallbackRoutes} {
		for _, route := range routes {
			if route.DiscoveredUpstream != "" && cfg.Docker == nil {
				return fmt.Errorf("discovered upstream %q requires Docker discovery", route.DiscoveredUpstream)
			}
		}
	}
	return nil
}

func (p *dockerProvider) boundServices() map[string]bool {
	out := make(map[string]bool, len(p.cfg.ServiceBindings))
	for name := range p.cfg.ServiceBindings {
		out[name] = true
	}
	return out
}

func (p *dockerProvider) referencedServices() map[string]bool {
	names := map[string]bool{}
	if p.srv == nil || p.srv.cfg == nil {
		return names
	}
	for _, routes := range [][]*resolved.Route{p.srv.cfg.Routes, p.srv.cfg.FallbackRoutes} {
		for _, route := range routes {
			if route.DiscoveredUpstream != "" {
				names[route.DiscoveredUpstream] = true
			}
		}
	}
	return names
}

func (p *dockerProvider) addBoundService(svc *docker.Service, next *dynamicTable) []docker.Matcher {
	binding, ok := p.cfg.ServiceBindings[svc.BoundService]
	if !ok || p.srv == nil {
		return p.refuse(next, svc.Name, svc.Routes)
	}
	var pool *resolved.Pool
	base := binding.Handler
	if binding.Upstream != "" {
		pool = p.srv.cfg.Upstreams[binding.Upstream]
		base = p.srv.pools[binding.Upstream]
	}
	if isNilHandler(base) {
		return p.refuse(next, svc.Name, svc.Routes)
	}
	chains, tombs := p.routeChains(svc, next)
	p.appendServiceRoutes(svc.Name, pool, chains, base, nil, 0, "", next)
	return tombs
}

func (s *server) discoveredUpstream(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		table := s.dynamic.Load()
		if table == nil || table.serviceHandlers[name] == nil {
			http.Error(w, "no backends available", http.StatusServiceUnavailable)
			return
		}
		table.serviceHandlers[name].ServeHTTP(w, r)
	})
}

func validateDockerServiceBindings(cfg *resolved.Config) error {
	if cfg.Docker != nil {
		for name, binding := range cfg.Docker.ServiceBindings {
			if name == "" || (binding.Upstream != "") == binding.HandlerBinding {
				return fmt.Errorf("docker service binding %q: select one pool or handler", name)
			}
			if binding.Upstream != "" && cfg.Upstreams[binding.Upstream] == nil {
				return fmt.Errorf("docker service binding %q: unknown upstream", name)
			}
			if binding.HandlerBinding && isNilHandler(binding.Handler) {
				return fmt.Errorf("docker service binding %q: nil handler", name)
			}
		}
	}
	return nil
}
