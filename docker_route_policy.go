package statute

import (
	"fmt"
	"reflect"

	"statute.kjanat.dev/internal/docker"
	"statute.kjanat.dev/resolved"
)

func validateServiceHints(svc *docker.Service) error {
	for _, m := range svc.Routes {
		if _, err := routeHints(svc.Name, m.Hints); err != nil {
			return err
		}
	}
	return nil
}

func (p *dockerProvider) resolveRouteMiddleware(svc *docker.Service, m docker.Matcher) ([]resolved.Middleware, string) {
	hints, err := routeHints(svc.Name, m.Hints)
	if err != nil {
		return nil, err.Error()
	}
	return p.routeMiddleware(svc, m, hints)
}

// dockerRoutePredicate identifies exact traffic without its policy provenance.
type dockerRoutePredicate struct {
	host     string
	hostKind docker.HostKind
	path     string
	pathKind docker.PathKind
}

type dockerRoutePolicyGroup struct {
	chain    routeChain
	conflict bool
}

func (p *dockerProvider) coalesceRouteChains(service string, chains []routeChain, next *dynamicTable) ([]routeChain, []docker.Matcher) {
	var groups []dockerRoutePolicyGroup
	indices := make(map[dockerRoutePredicate]int)
	for _, chain := range chains {
		m := chain.m
		key := dockerRoutePredicate{m.Host, m.HostKind, m.Path, m.PathKind}
		chain.mws = canonicalDockerMiddleware(chain.mws)
		if i, ok := indices[key]; ok {
			groups[i].conflict = groups[i].conflict || !reflect.DeepEqual(groups[i].chain.mws, chain.mws)
		} else {
			indices[key] = len(groups)
			groups = append(groups, dockerRoutePolicyGroup{chain: chain})
		}
	}
	var kept []routeChain
	var tombs []docker.Matcher
	for _, group := range groups {
		if group.conflict {
			m := group.chain.m
			p.warn([]string{fmt.Sprintf("service %q: conflicting middleware for route %s%s, refusing predicate", service, m.Host, m.Path)})
			tombs = append(tombs, p.refuse(next, service, []docker.Matcher{m})...)
			continue
		}
		kept = append(kept, group.chain)
	}
	return kept, tombs
}

// canonicalDockerMiddleware matches compression's set semantics while retaining
// wrapper order and independent copies of any normalized algorithm lists.
func canonicalDockerMiddleware(chain []resolved.Middleware) []resolved.Middleware {
	out := make([]resolved.Middleware, len(chain))
	copy(out, chain)
	for i := range out {
		if out[i].Type != resolved.MWCompress {
			continue
		}
		gzip, brotli := compressionAlgorithms(out[i].CompressAlgos)
		out[i].CompressAlgos = nil
		if gzip {
			out[i].CompressAlgos = append(out[i].CompressAlgos, resolved.Gzip)
		}
		if brotli {
			out[i].CompressAlgos = append(out[i].CompressAlgos, resolved.Brotli)
		}
	}
	return out
}
