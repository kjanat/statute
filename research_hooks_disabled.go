//go:build !htmlrewrite_research

package statute

import "net/http"

func researchDockerRoute(_, _, _ string, next http.Handler) http.Handler { return next }

func researchProxyTransport(base *http.Transport) http.RoundTripper { return base }

func researchBackendFailure(_ error) bool { return true }
