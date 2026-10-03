//go:build htmlrewrite_research

// Package researchroute provides a private route-to-transport test seam.
package researchroute

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
)

// Route identifies one compiled Docker route.
type Route struct{ Service, Host, Path string }

// Wrapper adapts a pool attempt with route-owned policy.
type Wrapper func(http.RoundTripper) http.RoundTripper

// Factory captures policy during generation construction.
type Factory func(Route) Wrapper

var registered atomic.Pointer[Factory]

// Install fixes the experiment's factory before constructing any server.
func Install(factory Factory) error {
	if factory == nil || !registered.CompareAndSwap(nil, &factory) {
		return errors.New("research route factory must be installed once before startup")
	}
	return nil
}

type routeKey struct{}

// Handler captures route policy during generation construction.
func Handler(route Route, next http.Handler) http.Handler {
	factory := registered.Load()
	if factory == nil {
		return next
	}
	wrap := (*factory)(route)
	if wrap == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, WithWrapper(r, wrap))
	})
}

// WithWrapper copies the request with its selected response-stage policy.
func WithWrapper(r *http.Request, wrap Wrapper) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), routeKey{}, wrap))
}

// Transport applies request-scoped policy around the pool-owned transport.
func Transport(base http.RoundTripper) http.RoundTripper { return &transport{base: base} }

type transport struct{ base http.RoundTripper }

func (t *transport) RoundTrip(r *http.Request) (*http.Response, error) {
	if wrap, ok := r.Context().Value(routeKey{}).(Wrapper); ok {
		attempt := &baseAttempt{base: t.base}
		res, err := wrap(attempt).RoundTrip(r)
		if err != nil && attempt.err == nil {
			return res, &ResponseError{Err: err}
		}
		return res, err
	}
	return t.base.RoundTrip(r)
}

// ResponseError identifies a route-stage failure after successful transport.
type ResponseError struct{ Err error }

func (e *ResponseError) Error() string { return e.Err.Error() }
func (e *ResponseError) Unwrap() error { return e.Err }

type baseAttempt struct {
	base http.RoundTripper
	err  error
}

func (a *baseAttempt) RoundTrip(r *http.Request) (*http.Response, error) {
	res, err := a.base.RoundTrip(r)
	a.err = err
	return res, err
}
