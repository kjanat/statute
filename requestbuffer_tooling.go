package statute

import (
	"fmt"
	"slices"

	"statute.kjanat.dev/resolved"
)

func requestBufferChains(c *resolved.Config, visit func(string, []resolved.Middleware)) {
	for i, r := range c.Routes {
		visit(fmt.Sprintf("routes[%d]", i), r.Middleware)
	}
	for i, r := range c.FallbackRoutes {
		visit(fmt.Sprintf("fallback_routes[%d]", i), r.Middleware)
	}
	if c.Docker == nil {
		return
	}
	visit("docker.default_middleware", c.Docker.DefaultMiddleware)
	names := make([]string, 0, len(c.Docker.Middleware))
	for name := range c.Docker.Middleware {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		visit(fmt.Sprintf("docker.middleware[%q]", name), c.Docker.Middleware[name])
	}
}

func graphRequestBuffers(d *dotWriter, c *resolved.Config) {
	resources := c.RequestBuffers
	label := fmt.Sprintf("RequestBuffer resources\\nmemory=%d\\ndisk=%d\\nstorage=%s\\nmax_in_flight=%d", resources.MemoryBudgetBytes, resources.DiskBudgetBytes, resources.Storage, resources.MaxInFlight)
	d.printf("  RB [shape=box, label=%q];\n", label)
	index := 0
	requestBufferChains(c, func(path string, chain []resolved.Middleware) {
		for i, mw := range chain {
			if mw.Type != resolved.MWRequestBuffer {
				continue
			}
			label := fmt.Sprintf("%s.middleware[%d]\\nRequestBuffer max=%d threshold=%d", path, i, mw.RequestBufferMaxBytes, mw.RequestBufferMemoryThresholdBytes)
			d.printf("  RB%d [shape=box, label=%q];\n  RB%d -> RB [label=\"uses shared limits\"];\n", index, label, index)
			index++
		}
	})
}

func ruleRequestBuffers(c *resolved.Config) []Finding {
	var out []Finding
	resources := c.RequestBuffers
	requestBufferChains(c, func(path string, chain []resolved.Middleware) {
		for i, mw := range chain {
			if mw.Type != resolved.MWRequestBuffer {
				continue
			}
			needed := mw.RequestBufferMaxBytes + 1
			if resources.Storage != "" {
				needed = mw.RequestBufferMemoryThresholdBytes + 1
			}
			if needed <= resources.MemoryBudgetBytes && (resources.Storage == "" || mw.RequestBufferMaxBytes <= resources.DiskBudgetBytes) {
				continue
			}
			out = append(out, Finding{Severity: SeverityWarning, Code: "RBF001", Path: fmt.Sprintf("%s.middleware[%d]", path, i), Message: fmt.Sprintf("RequestBuffer maximum=%d threshold=%d requires more space than server request_buffers allows: memory=%d disk=%d storage=%q; requests may receive 503 before reaching their route limit", mw.RequestBufferMaxBytes, mw.RequestBufferMemoryThresholdBytes, resources.MemoryBudgetBytes, resources.DiskBudgetBytes, resources.Storage)})
		}
	})
	return out
}
