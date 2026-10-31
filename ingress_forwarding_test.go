package statute

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
)

func TestIngressForwardedOperationsRunOnce(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		for _, tc := range []struct {
			name string
			ops  []Middleware
			want []string
		}{
			{"add", []Middleware{AddRequestHeader("X-Forwarded-For", "198.51.100.7")}, []string{"192.0.2.5", "198.51.100.7"}},
			{"set_add", []Middleware{SetRequestHeader("X-Forwarded-For", "198.51.100.8"), AddRequestHeader("X-Forwarded-For", "198.51.100.7")}, []string{"198.51.100.8", "198.51.100.7"}},
			{"remove_add", []Middleware{RemoveRequestHeader("X-Forwarded-For"), AddRequestHeader("X-Forwarded-For", "198.51.100.7")}, []string{"198.51.100.7"}},
		} {
			t.Run(tc.name+map[bool]string{false: "/direct", true: "/trusted"}[trusted], func(t *testing.T) {
				seen := make(chan []string, 1)
				origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					seen <- r.Header.Values("X-Forwarded-For")
					w.WriteHeader(http.StatusNoContent)
				}))
				defer origin.Close()
				listener := HTTP(":0").With(IngressPolicy{ForwardedHeaders: true})
				want := append([]string(nil), tc.want...)
				if trusted {
					listener.With(TrustedProxy("192.0.2.0/24"))
					if tc.name == "add" {
						want[0] = "203.0.113.4, 192.0.2.5"
					}
				}
				h := ingressTestHandler(t, Config{
					Listeners: Listeners{listener},
					Upstreams: Upstreams{"origin": Pool{Backends: []Backend{{Address: origin.URL}}}},
					Routes:    Routes{Match("/*").ProxyTo("origin").With(tc.ops...)},
				})
				r := httptest.NewRequest(http.MethodGet, "/", nil)
				r.RemoteAddr = "192.0.2.5:1234"
				r.Header.Set("X-Forwarded-For", "203.0.113.4")
				rec := runRequest(t, h, r)
				if rec.Code != http.StatusNoContent {
					t.Fatalf("status=%d", rec.Code)
				}
				if got := <-seen; !reflect.DeepEqual(got, want) {
					t.Fatalf("forwarded chain=%q want=%q", got, want)
				}
			})
		}
	}
}

func TestIngressNormalizationPreservesProxyRetryTrailers(t *testing.T) {
	for _, tc := range []struct {
		name string
		mws  []Middleware
		want int32
	}{
		{"direct", nil, 1},
		{"retry", []Middleware{Retry(2, OnStatus(502))}, 2},
		{"retry_admission_fallback", []Middleware{Retry(2).RequestBufferBudget("1B")}, 1},
		{"request_buffer_retry", []Middleware{RequestBuffer("1KiB"), Retry(2, OnStatus(502))}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != "pt" || r.Trailer.Get("X-Final") != "complete" || r.URL.Path != "/upload" {
					t.Errorf("origin: body=%q error=%v trailers=%v path=%s", body, err, r.Trailer, r.URL.Path)
				}
				if calls.Add(1) == 1 && tc.want == 2 {
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer origin.Close()
			h := ingressTestHandler(t, Config{
				Listeners: Listeners{HTTP(":0").With(IngressPolicy{NormalizePath: true, SanitizePath: true})},
				Upstreams: Upstreams{"origin": Pool{Backends: []Backend{{Address: origin.URL}}}},
				Routes:    Routes{Match("/upload").ProxyTo("origin").With(tc.mws...)},
			})
			r := httptest.NewRequest(http.MethodPut, "/a/../%75pload", nil)
			r.ContentLength = -1
			r.TransferEncoding = []string{"chunked"}
			r.Trailer = http.Header{"X-Final": nil}
			r.Body = &retryTrailerReplacingBody{request: r}
			rec := runRequest(t, h, r)
			if rec.Code != http.StatusNoContent || calls.Load() != tc.want {
				t.Fatalf("status=%d attempts=%d want=%d", rec.Code, calls.Load(), tc.want)
			}
		})
	}
}

func ingressTestHandler(t *testing.T, cfg Config) http.Handler {
	t.Helper()
	s, err := newServer(mustResolve(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, pool := range s.pools {
			pool.transport.CloseIdleConnections()
		}
	})
	return s.listeners[0].Handler
}
