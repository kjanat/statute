package statute

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"statute.kjanat.dev/resolved"
)

// GraphDOT writes a Graphviz DOT representation of the resolved config to w.
// Render with `dot -Tsvg < input.dot > topology.svg` (or any DOT-capable
// renderer).
//
// The graph has four kinds of nodes:
//
//   - Listeners (Mrecord, blue) — one per HTTP/HTTPS listener.
//   - Routes (rectangle, light yellow) — one per declared route.
//   - Upstream pools (ellipse, green) — one per named pool.
//   - Backends (circle, gray) — one per backend in each pool, dashed if Backup.
//
// Edges:
//
//   - Listener → Listener for redirect-to-https arrows.
//   - Listener → Route for the matching relation (every content listener
//     reaches every route; the host filter is on the route node).
//   - Route → Pool for ProxyTo.
//   - Pool → Backend for membership; weighted edges show Weight.
//
// The output is intentionally minimal — no fancy layout, no colour palette.
// Pipe it through dot with your preferred styling.
func GraphDOT(cfg Config, w io.Writer) error {
	r, err := Resolve(cfg)
	if err != nil {
		return err
	}
	return graphResolved(r, w)
}

func graphResolved(r *resolved.Config, w io.Writer) error {
	pf := func(format string, args ...any) error {
		_, err := fmt.Fprintf(w, format, args...)
		return err
	}
	if err := pf("digraph statute {\n"); err != nil {
		return err
	}
	if err := pf("  rankdir=LR;\n  node [fontname=\"Helvetica\"];\n  edge [fontname=\"Helvetica\"];\n\n"); err != nil {
		return err
	}

	// Listeners
	if err := pf("  // listeners\n"); err != nil {
		return err
	}
	for i, l := range r.Listeners {
		label := fmt.Sprintf("%s %s", strings.ToUpper(l.Scheme), l.Addr)
		if l.Redirect != "" {
			label = fmt.Sprintf("%s -> %s (redirect)", strings.ToUpper(l.Scheme), l.Redirect)
		}
		if err := pf("  L%d [shape=Mrecord, style=filled, fillcolor=\"#cfe2ff\", label=%q];\n", i, label); err != nil {
			return err
		}
	}

	// Redirect listener -> content listener edge: find the matching scheme.
	for i, l := range r.Listeners {
		if l.Redirect == "" {
			continue
		}
		for j, target := range r.Listeners {
			if target.Scheme == l.Redirect {
				if err := pf("  L%d -> L%d [label=\"redirect 301\", style=dashed];\n", i, j); err != nil {
					return err
				}
			}
		}
	}

	// Routes
	if err := pf("\n  // routes\n"); err != nil {
		return err
	}
	for i, route := range r.Routes {
		host := route.Host
		if host == "" {
			host = "*"
		}
		label := fmt.Sprintf("%s %s", host, route.Pattern)
		if err := pf("  R%d [shape=box, style=\"filled,rounded\", fillcolor=\"#fff3cd\", label=%q];\n", i, label); err != nil {
			return err
		}
	}

	// Listener->route edges: only from content listeners.
	for i, l := range r.Listeners {
		if l.Redirect != "" {
			continue
		}
		for j := range r.Routes {
			if err := pf("  L%d -> R%d [color=\"#888\"];\n", i, j); err != nil {
				return err
			}
		}
	}

	// Pools (sorted for stable output) and backends.
	poolNames := make([]string, 0, len(r.Upstreams))
	for name := range r.Upstreams {
		poolNames = append(poolNames, name)
	}
	sort.Strings(poolNames)

	if err := pf("\n  // upstreams\n"); err != nil {
		return err
	}
	for _, name := range poolNames {
		pool := r.Upstreams[name]
		strategyName := strategyString(pool.Strategy)
		label := fmt.Sprintf("%s\\n(%s)", name, strategyName)
		if err := pf("  P_%s [shape=ellipse, style=filled, fillcolor=\"#d1e7dd\", label=%q];\n", sanitize(name), label); err != nil {
			return err
		}
		for k, b := range pool.Backends {
			style := "solid"
			color := "#dee2e6"
			if b.Backup {
				style = "dashed"
				color = "#e2e3e5"
			}
			if err := pf("  B_%s_%d [shape=circle, style=\"filled,%s\", fillcolor=%q, label=%q];\n",
				sanitize(name), k, style, color, b.Address); err != nil {
				return err
			}
			edgeLabel := ""
			if b.Weight != 1 {
				edgeLabel = fmt.Sprintf("w=%d", b.Weight)
			}
			if err := pf("  P_%s -> B_%s_%d [label=%q];\n", sanitize(name), sanitize(name), k, edgeLabel); err != nil {
				return err
			}
		}
	}

	// Route -> pool edges
	for i, route := range r.Routes {
		if route.Upstream == nil {
			continue
		}
		if err := pf("  R%d -> P_%s;\n", i, sanitize(route.Upstream.Name)); err != nil {
			return err
		}
	}

	return pf("}\n")
}

// sanitize turns an arbitrary string into a valid DOT identifier suffix by
// replacing every non-alphanumeric character with underscore.
func sanitize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}

func strategyString(s resolved.Strategy) string {
	switch s {
	case resolved.RoundRobin:
		return "round-robin"
	case resolved.LeastConnections:
		return "least-conn"
	case resolved.IPHash:
		return "ip-hash"
	case resolved.Weighted:
		return "weighted"
	default:
		return enumUnknown
	}
}
