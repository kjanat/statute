//go:build htmlrewrite_research

package htmlrewrite

import (
	"net/http"
	"os"
	"testing"

	"statute.kjanat.dev"
	"statute.kjanat.dev/internal/researchroute"
)

func configureHTTPDockerExperiment(t *testing.T, cfg *statute.Config, e *httpEngine) {
	t.Helper()
	endpoint := os.Getenv("STATUTE_HTML_DOCKER_ENDPOINT")
	if endpoint == "" {
		return
	}
	end := newRewriteEndBarrier()
	if err := researchroute.Install(func(route researchroute.Route) researchroute.Wrapper {
		if route.Host == "plain.test" {
			return nil
		}
		return func(base http.RoundTripper) http.RoundTripper {
			p := testHTTPPolicy(failClosed)
			if route.Host == "open.test" {
				p.failure = failOpen
			}
			return end.wrap(httpTestTransport(t, e, base, p))
		}
	}); err != nil {
		t.Fatal(err)
	}
	cfg.Routes = statute.Routes{statute.Match("/_research/end").Handle(end)}
	cfg.Docker = statute.Docker().Endpoint(endpoint).Refresh("25ms").Storage(os.Getenv("STATUTE_HTML_DOCKER_STORAGE")).
		Workload("page", statute.WorkloadPolicy{IdleAfter: "200ms", Readiness: statute.TCPReadiness})
}
