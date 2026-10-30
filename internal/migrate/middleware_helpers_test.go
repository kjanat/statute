package migrate

import "statute.kjanat.dev/resolved"

func testMiddlewareLeaves(values []resolved.Middleware) []resolved.Middleware {
	var out []resolved.Middleware
	for _, m := range values {
		if m.Type == resolved.MWInOrder || m.Type == resolved.MWOnListeners {
			out = append(out, testMiddlewareLeaves(m.Group)...)
		} else {
			out = append(out, m)
		}
	}
	return out
}
