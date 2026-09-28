package statute

import (
	"errors"
	"fmt"
	"strings"

	"statute.kjanat.dev/resolved"
)

// resolveRouteExpansion lowers Hosts through the ordinary single-host resolver.
// Each pass allocates independent resolved values while retaining intentional
// references to the same upstream pool or opaque handler.
func resolveRouteExpansion(route *Route, pools map[string]*resolved.Pool) ([]*resolved.Route, error) {
	if !route.hostsSet {
		resolvedRoute, err := resolveRoute(route, pools)
		if err != nil {
			return nil, err
		}
		return []*resolved.Route{resolvedRoute}, nil
	}
	if err := validateRouteHosts(route); err != nil {
		return nil, err
	}
	routes := make([]*resolved.Route, 0, len(route.hosts))
	for i, host := range route.hosts {
		concrete := *route
		concrete.host = host
		resolvedRoute, err := resolveRoute(&concrete, pools)
		if err != nil {
			return nil, fmt.Errorf("host[%d] %q: %w", i, host, err)
		}
		routes = append(routes, resolvedRoute)
	}
	return routes, nil
}

// validateRouteHosts checks declaration intent and the existing matcher's
// case-fold equivalence without adding a separate hostname syntax policy.
func validateRouteHosts(route *Route) error {
	if route.hostSet {
		return errors.New("route cannot combine Host and Hosts")
	}
	if len(route.hosts) == 0 {
		return errors.New("hosts: at least one host required")
	}
	for i, host := range route.hosts {
		if host == "" {
			return fmt.Errorf("host[%d] %q: host must not be empty", i, host)
		}
		for j, previous := range route.hosts[:i] {
			if strings.EqualFold(previous, host) {
				return fmt.Errorf("host[%d] %q duplicates host[%d] %q (case-insensitive)", i, host, j, previous)
			}
		}
	}
	return nil
}
