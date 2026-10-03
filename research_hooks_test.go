//go:build htmlrewrite_research

package statute

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"statute.kjanat.dev/internal/researchroute"
	"statute.kjanat.dev/resolved"
)

type researchRoundTrip func(*http.Request) (*http.Response, error)

func (f researchRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestResearchRouteFailureDoesNotDemotePool(t *testing.T) {
	for _, reject := range []bool{false, true} {
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if reject {
				w.WriteHeader(200)
			} else {
				w.WriteHeader(503)
			}
		}))
		t.Cleanup(origin.Close)
		u, err := url.Parse(origin.URL)
		if err != nil {
			t.Fatal(err)
		}
		transport := &http.Transport{}
		t.Cleanup(transport.CloseIdleConnections)
		failures := 0
		proxy := newBackendProxy(u, transport, &resolved.Pool{}, func(*http.Request) { failures++ })
		req := httptest.NewRequest("GET", "http://route.test/", nil)
		req = researchroute.WithWrapper(req, func(base http.RoundTripper) http.RoundTripper {
			return researchRoundTrip(func(r *http.Request) (*http.Response, error) {
				res, err := base.RoundTrip(r)
				if err != nil || !reject {
					return res, err
				}
				_ = res.Body.Close()
				return nil, errors.New("route declined this representation")
			})
		})
		res := httptest.NewRecorder()
		proxy.ServeHTTP(res, req)
		wantFailures, wantStatus := 1, 503
		if reject {
			wantFailures, wantStatus = 0, 502
		}
		if failures != wantFailures || res.Code != wantStatus {
			t.Fatalf("reject=%v: backend failures=%d status=%d", reject, failures, res.Code)
		}
	}
}
