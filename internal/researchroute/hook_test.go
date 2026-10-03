//go:build htmlrewrite_research

package researchroute

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFailureOwnership(t *testing.T) {
	for _, upstreamFailure := range []bool{false, true} {
		cause := errors.New("failure")
		base := roundTripFunc(func(*http.Request) (*http.Response, error) {
			if upstreamFailure {
				return nil, cause
			}
			return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
		})
		wrap := Wrapper(func(base http.RoundTripper) http.RoundTripper {
			return roundTripFunc(func(r *http.Request) (*http.Response, error) {
				res, err := base.RoundTrip(r)
				if err != nil {
					return nil, err
				}
				_ = res.Body.Close()
				return nil, cause
			})
		})
		req := httptest.NewRequest("GET", "http://test/", nil)
		req = req.WithContext(context.WithValue(req.Context(), routeKey{}, wrap))
		_, err := Transport(base).RoundTrip(req)
		var rejected *ResponseError
		if !errors.Is(err, cause) || errors.As(err, &rejected) == upstreamFailure {
			t.Fatalf("wrong failure owner: upstream=%v, error=%v", upstreamFailure, err)
		}
	}
}
