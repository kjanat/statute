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
// The graph has six kinds of nodes:
//
//   - Listeners (Mrecord, blue) — one per HTTP/HTTPS listener.
//   - Routes (rectangle, light yellow) — one per declared route.
//   - Upstream pools (ellipse, green) — one per named pool.
//   - Backends (circle, gray) — one per backend in each pool, dashed if Backup.
//   - Docker pool policies (ellipse, dashed green): one per code-owned policy.
//   - The fallback (rectangle, light red): one, when Config.Fallback is set.
//
// Edges:
//
//   - Listener → Listener for redirect-to-https arrows.
//   - Listener → Route for the matching relation (every content listener
//     reaches every route; the host filter is on the route node).
//   - Listener → Fallback, dashed, from the same content listeners
//     (terminal stage after routes and Docker tombstones miss).
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

// dotWriter accumulates the first write error so the graph emitters can be
// written as straight-line code instead of an if-err ladder after every
// Fprintf. Once err is set, subsequent printf calls are no-ops.
type dotWriter struct {
	w   io.Writer
	err error
}

func (d *dotWriter) printf(format string, args ...any) {
	if d.err != nil {
		return
	}
	_, d.err = fmt.Fprintf(d.w, format, args...)
}

func graphResolved(r *resolved.Config, w io.Writer) error {
	d := &dotWriter{w: w}
	d.printf("digraph statute {\n")
	d.printf("  rankdir=LR;\n  node [fontname=\"Helvetica\"];\n  edge [fontname=\"Helvetica\"];\n\n")
	graphListeners(d, r)
	graphRoutes(d, r)
	graphFallback(d, r)
	graphUpstreams(d, r)
	graphDockerPoolPolicies(d, r)
	d.printf("}\n")
	return d.err
}

func graphDockerPoolPolicies(d *dotWriter, r *resolved.Config) {
	if r.Docker == nil || len(r.Docker.PoolPolicy) == 0 {
		return
	}
	names := make([]string, 0, len(r.Docker.PoolPolicy))
	for name := range r.Docker.PoolPolicy {
		names = append(names, name)
	}
	sort.Strings(names)

	d.printf("\n  // docker pool policies\n")
	for i, name := range names {
		policy := r.Docker.PoolPolicy[name]
		label := fmt.Sprintf("%s\\nDocker pool policy\\nhost=%s\\nhealth=%s\\ntransport=%s",
			name, resolvedHostPolicyString(policy.UpstreamHost, policy.HostValue),
			healthPolicyString(policy.HealthCheck, policy.PassiveHealthCheck),
			transportPolicyString(policy.Transport))
		d.printf("  DP_%d [shape=ellipse, style=\"filled,dashed\", fillcolor=\"#d1e7dd\", label=%q];\n", i, label)
	}
}

func resolvedHostPolicyString(policy resolved.HostPolicy, value string) string {
	switch policy {
	case resolved.HostClient:
		return "client"
	case resolved.HostTarget:
		return "target"
	case resolved.HostExplicit:
		return value
	default:
		return enumUnknown
	}
}

func healthPolicyString(active resolved.HealthCheck, passive resolved.PassiveHealthCheck) string {
	var parts []string
	if active.Enabled {
		parts = append(parts, fmt.Sprintf("active(path=%s,interval=%s,timeout=%s,healthy=%d,unhealthy=%d,host=%s,statuses=%v)",
			active.Path, active.Interval, active.Timeout, active.Healthy, active.Unhealthy, active.Host, active.Statuses))
	}
	if passive.Enabled {
		parts = append(parts, fmt.Sprintf("passive(window=%s,max=%d)", passive.FailureWindow, passive.MaxFailures))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "+")
}

func transportPolicyString(transport resolved.Transport) string {
	tlsPolicy := "system-tls"
	if transport.InsecureSkipVerify {
		tlsPolicy = "insecure-tls"
	} else if transport.ServerName != "" || len(transport.RootCAFiles) > 0 {
		tlsPolicy = "custom-tls"
	}
	parts := []string{
		tlsPolicy,
		"sni=" + transport.ServerName,
		fmt.Sprintf("roots=%v", transport.RootCAFiles),
		fmt.Sprintf("max-idle=%d", transport.MaxIdleConnsPerHost),
		"idle=" + transport.IdleConnTimeout.String(),
		"dial=" + transport.DialTimeout.String(),
		"handshake=" + transport.TLSHandshakeTimeout.String(),
		"header=" + transport.ResponseHeaderTimeout.String(),
		"flush=" + transport.FlushInterval.String(),
	}
	return strings.Join(parts, ",")
}

func graphListeners(d *dotWriter, r *resolved.Config) {
	d.printf("  // listeners\n")
	for i, l := range r.Listeners {
		label := fmt.Sprintf("%s %s", strings.ToUpper(l.Scheme), l.Addr)
		if l.Redirect != "" {
			label = fmt.Sprintf("%s -> %s (redirect)", strings.ToUpper(l.Scheme), l.Redirect)
		}
		if l.ClientAuth != nil {
			label += fmt.Sprintf("\\nclient-auth=%s\\nclient-ca=%v", l.ClientAuth.Mode, l.ClientAuth.CAFiles)
		}
		d.printf("  L%d [shape=Mrecord, style=filled, fillcolor=\"#cfe2ff\", label=%q];\n", i, label)
	}

	// Redirect listener -> content listener edge: find the matching scheme.
	for i, l := range r.Listeners {
		if l.Redirect == "" {
			continue
		}
		for j, target := range r.Listeners {
			if target.Scheme == l.Redirect {
				d.printf("  L%d -> L%d [label=\"redirect 301\", style=dashed];\n", i, j)
			}
		}
	}
}

func graphRoutes(d *dotWriter, r *resolved.Config) {
	d.printf("\n  // routes\n")
	for i, route := range r.Routes {
		host := route.Host
		if host == "" {
			host = "*"
		}
		label := fmt.Sprintf("%s %s", host, route.Pattern)
		d.printf("  R%d [shape=box, style=\"filled,rounded\", fillcolor=\"#fff3cd\", label=%q];\n", i, label)
	}

	// Listener->route edges: only from content listeners.
	for i, l := range r.Listeners {
		if l.Redirect != "" {
			continue
		}
		for j := range r.Routes {
			d.printf("  L%d -> R%d [color=\"#888\"];\n", i, j)
		}
	}
}

// graphFallback renders the terminal fallback stage, reached from the same
// content listeners the routes are, and from no redirect-only listener. It
// is reached only when no route and no tombstone of the current Docker
// generation matched; neither the generation's routes nor its tombstones
// are in the resolved model, so neither is in the graph.
func graphFallback(d *dotWriter, r *resolved.Config) {
	if !r.HasFallback {
		return
	}
	d.printf("\n  // fallback\n")
	d.printf("  F [shape=box, style=\"filled,rounded\", fillcolor=\"#f8d7da\", label=\"fallback\"];\n")
	for i, l := range r.Listeners {
		if l.Redirect != "" {
			continue
		}
		d.printf("  L%d -> F [color=\"#888\", style=dashed];\n", i)
	}
}

func graphUpstreams(d *dotWriter, r *resolved.Config) {
	// Pools (sorted for stable output) and backends.
	poolNames := make([]string, 0, len(r.Upstreams))
	for name := range r.Upstreams {
		poolNames = append(poolNames, name)
	}
	sort.Strings(poolNames)

	d.printf("\n  // upstreams\n")
	for _, name := range poolNames {
		pool := r.Upstreams[name]
		label := fmt.Sprintf("%s\\n(%s)", name, strategyString(pool.Strategy))
		d.printf("  P_%s [shape=ellipse, style=filled, fillcolor=\"#d1e7dd\", label=%q];\n", sanitize(name), label)
		graphBackends(d, name, pool)
	}

	// Route -> pool edges
	for i, route := range r.Routes {
		if route.Upstream == nil {
			continue
		}
		d.printf("  R%d -> P_%s;\n", i, sanitize(route.Upstream.Name))
	}
}

func graphBackends(d *dotWriter, name string, pool *resolved.Pool) {
	for k, b := range pool.Backends {
		style := "solid"
		color := "#dee2e6"
		if b.Backup {
			style = "dashed"
			color = "#e2e3e5"
		}
		d.printf("  B_%s_%d [shape=circle, style=\"filled,%s\", fillcolor=%q, label=%q];\n",
			sanitize(name), k, style, color, b.Address)
		edgeLabel := ""
		if b.Weight != 1 {
			edgeLabel = fmt.Sprintf("w=%d", b.Weight)
		}
		d.printf("  P_%s -> B_%s_%d [label=%q];\n", sanitize(name), sanitize(name), k, edgeLabel)
	}
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
