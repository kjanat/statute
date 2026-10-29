package statute

import (
	"fmt"
	"net/http"
	"slices"

	"statute.kjanat.dev/resolved"
)

type orderedMW struct{ values []Middleware }

func (*orderedMW) statuteMiddleware() {}

// InOrder executes a group at its declared position. Header operations and path
// rewrites run at the group's edge, rather than at the edge of the whole route.
// Retry outside the group invokes it again for each attempt.
func InOrder(mws ...Middleware) *orderedMW {
	return &orderedMW{values: append([]Middleware(nil), mws...)}
}

func (m *orderedMW) resolve() (resolved.Middleware, error) {
	values, err := resolveMiddlewares(m.values)
	if err != nil {
		return resolved.Middleware{}, fmt.Errorf("in_order: %w", err)
	}
	return resolved.Middleware{Type: resolved.MWInOrder, Group: values}, nil
}

func middlewareLeaves(mws []resolved.Middleware) []resolved.Middleware {
	var out []resolved.Middleware
	for _, m := range mws {
		if m.Type == resolved.MWInOrder || m.Type == resolved.MWOnListeners {
			out = append(out, middlewareLeaves(m.Group)...)
		} else {
			out = append(out, m)
		}
	}
	return out
}

func orderedMiddlewareHandler(m resolved.Middleware, next http.Handler, bypassCache bool) http.Handler {
	values := m.Group
	if bypassCache {
		values = middlewareWithoutCache(values)
	}
	handler := wrapRouteMiddleware(values, next, false)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out := r.Clone(r.Context())
		out.Trailer = r.Trailer
		handler.ServeHTTP(w, out)
	})
}

func middlewareWithoutCache(mws []resolved.Middleware) []resolved.Middleware {
	out := make([]resolved.Middleware, 0, len(mws))
	for _, m := range mws {
		if m.Type == resolved.MWCache {
			continue
		}
		if m.Type == resolved.MWInOrder || m.Type == resolved.MWOnListeners {
			m.Group = middlewareWithoutCache(m.Group)
		}
		out = append(out, m)
	}
	return out
}

type listenerMiddleware struct {
	names  []string
	values []Middleware
}

func (*listenerMiddleware) statuteMiddleware() {}

// OnListeners applies route middleware only on the named listeners. Each
// compiled route owns its own middleware instances for each listener group.
func OnListeners(names []string, mws ...Middleware) *listenerMiddleware {
	return &listenerMiddleware{names: append([]string(nil), names...), values: append([]Middleware(nil), mws...)}
}

func (m *listenerMiddleware) resolve() (resolved.Middleware, error) {
	if len(m.names) == 0 {
		return resolved.Middleware{}, fmt.Errorf("on_listeners: at least one listener name required")
	}
	values, err := resolveMiddlewares(m.values)
	if err != nil {
		return resolved.Middleware{}, err
	}
	return resolved.Middleware{Type: resolved.MWOnListeners, Group: values, MiddlewareListeners: append([]string(nil), m.names...)}, nil
}

func conditionalListenerMiddleware(m resolved.Middleware, next http.Handler, bypass bool) http.Handler {
	handler := orderedMiddlewareHandler(m, next, bypass)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if listenerMatches(m.MiddlewareListeners, r) {
			handler.ServeHTTP(w, r)
		} else {
			next.ServeHTTP(w, r)
		}
	})
}

func middlewareForListener(mws []resolved.Middleware, name string) []resolved.Middleware {
	var out []resolved.Middleware
	for _, m := range mws {
		if m.Type == resolved.MWOnListeners && !slices.Contains(m.MiddlewareListeners, name) {
			continue
		}
		if m.Type == resolved.MWInOrder || m.Type == resolved.MWOnListeners {
			out = append(out, middlewareForListener(m.Group, name)...)
		} else {
			out = append(out, m)
		}
	}
	return out
}

func validateResolvedMiddlewares(mws []resolved.Middleware) error {
	names := map[string]bool{"": true}
	var collect func([]resolved.Middleware)
	collect = func(values []resolved.Middleware) {
		for _, m := range values {
			for _, name := range m.MiddlewareListeners {
				names[name] = true
			}
			collect(m.Group)
		}
	}
	collect(mws)
	for name := range names {
		chain := middlewareForListener(mws, name)
		for _, validate := range []func([]resolved.Middleware) error{validateMiddlewareRepresentation, validateCachePolicyOrder, validateRequestIDOwnership, validateBasicAuthCredentials} {
			if err := validate(chain); err != nil {
				return err
			}
		}
	}
	return nil
}
