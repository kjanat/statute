//go:build htmlrewrite_research

package statute

import (
	"errors"
	"net/http"

	"statute.kjanat.dev/internal/researchroute"
)

func researchDockerRoute(service, host, path string, next http.Handler) http.Handler {
	return researchroute.Handler(researchroute.Route{Service: service, Host: host, Path: path}, next)
}

func researchProxyTransport(base *http.Transport) http.RoundTripper {
	return researchroute.Transport(base)
}

func researchBackendFailure(err error) bool {
	var rejected *researchroute.ResponseError
	return !errors.As(err, &rejected)
}
