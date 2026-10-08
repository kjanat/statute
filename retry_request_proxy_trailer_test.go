package statute

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRetryRequestBudgetProxyTrailers(t *testing.T) {
	origin := retryProxyTrailerOrigin(t)
	for _, tc := range []struct {
		name       string
		middleware []Middleware
	}{
		{"buffered", []Middleware{Retry(2).RequestBufferBudget("4KiB")}},
		{"fallback", []Middleware{Retry(2).RequestBufferBudget("1B")}},
		{"fallback_body_limit", []Middleware{Retry(2).RequestBufferBudget("1B"), BodyLimit("4KiB")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := fallbackRouter(t, Config{Listeners: Listeners{HTTP(":0")},
				Upstreams: Upstreams{"origin": Pool{Backends: []Backend{{Address: origin.URL}}}},
				Routes:    Routes{Match("/*").ProxyTo("origin").With(tc.middleware...)},
			})
			r := httptest.NewRequest(http.MethodPut, "/", nil)
			assertRetryProxyTrailers(t, h, r)
		})
	}
}

func retryProxyTrailerOrigin(t *testing.T) *httptest.Server {
	t.Helper()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body, err := io.ReadAll(r.Body); err != nil || string(body) != "pt" {
			t.Errorf("body=%q error=%v", body, err)
		}
		if r.Trailer.Get("X-Final") == "complete" {
			_, _ = io.WriteString(w, "present")
			return
		}
		_, _ = io.WriteString(w, "absent")
	}))
	t.Cleanup(origin.Close)
	return origin
}

func assertRetryProxyTrailers(t *testing.T, h http.Handler, r *http.Request) {
	t.Helper()
	r.ContentLength = -1
	r.Trailer = http.Header{"X-Final": nil}
	r.Body = &retryTrailerReplacingBody{request: r}
	response := runRequest(t, h, r)
	if response.Code != http.StatusOK || response.Body.String() != "present" {
		t.Fatalf("status=%d trailer report=%q", response.Code, response.Body.String())
	}
}
