package statute

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"statute.kjanat.dev/internal/docker"
)

func TestCacheFreshnessBoundaryOriginPolicy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, policy string
		wantCalls    int
	}{
		{"expired must revalidate", "max-age=0, must-revalidate", 2},
		{"configured TTL without origin freshness", "", 1},
		{"fresh origin", "max-age=3600", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				if tc.policy != "" {
					w.Header().Set("Cache-Control", tc.policy)
				}
				_, _ = fmt.Fprint(w, "origin")
			}), Cache("1h"))
			for range 2 {
				if got := runRequest(t, h, httptest.NewRequest("GET", "/", nil)); got.Code != 200 || got.Body.String() != "origin" {
					t.Fatalf("producer delivery changed: %d %q", got.Code, got.Body.String())
				}
			}
			if calls != tc.wantCalls {
				t.Fatalf("origin calls = %d, want %d", calls, tc.wantCalls)
			}
		})
	}
}

func TestCacheFreshnessBoundaryElapsedLifetime(t *testing.T) {
	for _, phase := range []string{"producer", "delivery"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls++
					w.Header().Set("Cache-Control", "max-age=1")
					w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
					w.WriteHeader(http.StatusOK)
					if phase == "producer" {
						time.Sleep(2 * time.Second)
					}
					_, _ = io.WriteString(w, "body")
				}), Cache("1h"))
				first := &freshnessDelayedWriter{ResponseRecorder: httptest.NewRecorder(), delay: phase == "delivery"}
				h.ServeHTTP(first, httptest.NewRequest("GET", "/", nil))
				second := runRequest(t, h, httptest.NewRequest("GET", "/", nil))
				if first.Body.String() != "body" || second.Body.String() != "body" || calls != 2 {
					t.Fatalf("%s reset expiry: first=%q second=%q calls=%d", phase, first.Body.String(), second.Body.String(), calls)
				}
			})
		})
	}
}

type freshnessDelayedWriter struct {
	*httptest.ResponseRecorder
	delay bool
}

func (w *freshnessDelayedWriter) Write(p []byte) (int, error) {
	if w.delay {
		time.Sleep(2 * time.Second)
	}
	return w.ResponseRecorder.Write(p)
}

func TestCacheFreshnessBoundaryTTLAndAge(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			w.Header().Set("Cache-Control", "max-age=3600")
			w.Header().Set("Date", time.Now().Add(-10*time.Second).UTC().Format(http.TimeFormat))
			w.Header().Set("Age", "20")
			_, _ = io.WriteString(w, "body")
		}), Cache("3s"))
		runRequest(t, h, httptest.NewRequest("GET", "/", nil))
		time.Sleep(2 * time.Second)
		got := runRequest(t, h, httptest.NewRequest("GET", "/", nil))
		if got.Header().Get("Age") != "22" || calls != 1 {
			t.Fatalf("resident age: Age=%q calls=%d", got.Header().Get("Age"), calls)
		}
		time.Sleep(time.Second)
		runRequest(t, h, httptest.NewRequest("GET", "/", nil))
		if calls != 2 {
			t.Fatalf("origin freshness extended configured TTL: calls=%d", calls)
		}
	})
}

func TestCacheFreshnessBoundaryResponseDelayAndCopies(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		start := time.Now()
		h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			time.Sleep(2 * time.Second)
			w.Header().Set("Cache-Control", "max-age=60")
			w.Header().Set("Age", "10")
			w.WriteHeader(http.StatusOK)
			time.Sleep(3 * time.Second)
			_, _ = io.WriteString(w, "body")
		}), Cache("1h"))
		first := runRequest(t, h, httptest.NewRequest("GET", "/", nil))
		date := start.Add(2 * time.Second).UTC().Format(http.TimeFormat)
		if first.Header().Get("Age") != "15" || first.Header().Get("Date") != date {
			t.Fatalf("response delay/body time missing: %v", first.Header())
		}
		first.Header()["Age"][0] = "99999"
		time.Sleep(2 * time.Second)
		results := make(chan *httptest.ResponseRecorder, 8)
		for range cap(results) {
			go func() {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
				results <- rec
			}()
		}
		for range cap(results) {
			rec := <-results
			if rec.Header().Get("Age") != "17" || rec.Header().Get("Date") != date {
				t.Fatalf("shared age snapshot corrupted: %v", rec.Header())
			}
			rec.Header()["Age"][0] = "99999"
		}
		if calls.Load() != 1 {
			t.Fatalf("fresh representation lost reuse: calls=%d", calls.Load())
		}
	})
}

func TestCacheFreshnessBoundaryStaticFallbackIsolation(t *testing.T) {
	t.Parallel()
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprintf("fallback=%t", fallback), func(t *testing.T) {
			calls := map[string]int{}
			base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls[r.URL.Path]++
				w.Header().Set("Cache-Control", "max-age=3600")
				_, _ = fmt.Fprint(w, calls[r.URL.Path])
			})
			routes := Routes{
				Match("/fresh").Handle(base).With(Cache("1h")),
				Match("/stale").Handle(base).With(Cache("1h"), SetResponseHeader("Cache-Control", "max-age=0")),
				Match("/age").Handle(base).With(Cache("1h"), RemoveResponseHeader("Age")),
			}
			cfg := Config{Listeners: Listeners{HTTP(":0")}, Routes: routes}
			if fallback {
				cfg.Routes, cfg.FallbackRoutes = nil, routes
			}
			h := fallbackRouter(t, cfg)
			for range 2 {
				for _, path := range []string{"/fresh", "/stale", "/age"} {
					runRequest(t, h, httptest.NewRequest("GET", path, nil))
				}
			}
			if calls["/fresh"] != 1 || calls["/stale"] != 2 || calls["/age"] != 2 {
				t.Fatalf("route freshness leaked: %v", calls)
			}
		})
	}
}

func TestCacheFreshnessBoundaryDockerSiblingIsolation(t *testing.T) {
	t.Parallel()
	cfg, err := resolveDocker(Docker().DefaultMiddleware(Cache("1h")).
		Middleware("stale", SetResponseHeader("Cache-Control", "max-age=0")).
		Middleware("age", RemoveResponseHeader("Age")))
	if err != nil {
		t.Fatal(err)
	}
	p := &dockerProvider{cfg: cfg}
	service := &docker.Service{Name: "shared"}
	for _, tc := range []struct {
		name  string
		names []string
		want  int
	}{
		{"fresh", nil, 1}, {"stale", []string{"stale"}, 2}, {"age", []string{"age"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mws, warning := p.routeMiddleware(service, docker.Matcher{Middlewares: tc.names}, nil)
			if warning != "" {
				t.Fatal(warning)
			}
			calls := 0
			h := wrapMiddleware(mws, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.Header().Set("Cache-Control", "max-age=3600")
				_, _ = io.WriteString(w, "body")
			}))
			for range 2 {
				runRequest(t, h, httptest.NewRequest("GET", "/", nil))
			}
			if calls != tc.want {
				t.Fatalf("Docker route freshness: calls=%d want=%d", calls, tc.want)
			}
		})
	}
}

func TestCacheFreshnessBoundaryProjection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, origin string
		op           Middleware
	}{
		{"remove expired policy", "max-age=0", RemoveResponseHeader("Cache-Control")},
		{"extend expired policy", "max-age=0", SetResponseHeader("Cache-Control", "max-age=3600")},
		{"shorten fresh policy", "max-age=3600", SetResponseHeader("Cache-Control", "max-age=0")},
		{"add explicit policy", "", SetResponseHeader("Cache-Control", "max-age=0")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				if tc.origin != "" {
					w.Header().Set("Cache-Control", tc.origin)
				}
				_, _ = io.WriteString(w, "body")
			}), Cache("1h"), tc.op)
			for range 2 {
				runRequest(t, h, httptest.NewRequest("GET", "/", nil))
			}
			if calls != 2 {
				t.Fatalf("header projection extended freshness: calls=%d", calls)
			}
		})
	}
}

func TestCacheFreshnessBoundaryRequestDirectives(t *testing.T) {
	t.Parallel()
	for _, directive := range []string{"max-age=0", "max-age=3600", "min-fresh=1", "max-stale", "max-stale=3600", "MAX-AGE=0"} {
		t.Run(directive, func(t *testing.T) {
			calls := 0
			h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				_, _ = fmt.Fprint(w, calls)
			}), Cache("1h"), Retry(2), ETag())
			runRequest(t, h, httptest.NewRequest("GET", "/", nil))
			r := httptest.NewRequest("GET", "/", nil)
			r.Header["cache-control"] = []string{directive}
			if got := runRequest(t, h, r); got.Body.String() != "2" {
				t.Fatalf("request freshness constraint used warm entry: %q", got.Body.String())
			}
			if got := runRequest(t, h, httptest.NewRequest("GET", "/", nil)); got.Body.String() != "1" || calls != 2 {
				t.Fatalf("request freshness constraint replaced warm entry: %q calls=%d", got.Body.String(), calls)
			}
		})
	}
}

func TestCacheFreshnessBoundaryAgeDateOperations(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"Age", "Date"} {
		for _, op := range []Middleware{SetResponseHeader(name, "0"), AddResponseHeader(name, "0"), RemoveResponseHeader(name)} {
			t.Run(fmt.Sprintf("%s/%T", name, op), func(t *testing.T) {
				calls := 0
				h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls++
					_, _ = fmt.Fprint(w, calls)
				}), Cache("1h"), op)
				runRequest(t, h, httptest.NewRequest("GET", "/", nil))
				if got := runRequest(t, h, httptest.NewRequest("GET", "/", nil)); got.Body.String() != "2" || calls != 2 {
					t.Fatalf("metadata override allowed cache reuse: %q calls=%d", got.Body.String(), calls)
				}
			})
		}
	}
}

func TestCacheFreshnessBoundaryCommittedPolicy(t *testing.T) {
	t.Parallel()
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit=%t", explicit), func(t *testing.T) {
			calls := 0
			h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.Header().Set("Cache-Control", "max-age=0, must-revalidate")
				if explicit {
					w.WriteHeader(http.StatusOK)
				}
				_, _ = io.WriteString(w, "body")
				w.Header().Del("Cache-Control")
			}), Cache("1h"))
			for range 2 {
				runRequest(t, h, httptest.NewRequest("GET", "/", nil))
			}
			if calls != 2 {
				t.Fatalf("late mutation erased committed freshness: calls=%d", calls)
			}
		})
	}
}

func TestCacheFreshnessBoundaryMiddlewareOrder(t *testing.T) {
	t.Parallel()
	for _, other := range []Middleware{ETag(), Retry(2, OnStatus(503)), Compress(Gzip)} {
		for _, cacheFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%T/cache-first=%t", other, cacheFirst), func(t *testing.T) {
				mws := []Middleware{Cache("1h"), other}
				if !cacheFirst {
					mws[0], mws[1] = mws[1], mws[0]
				}
				calls := 0
				h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls++
					w.Header().Set("Cache-Control", "max-age=0, must-revalidate")
					w.Header().Set("Content-Type", "text/plain")
					_, _ = io.WriteString(w, strings.Repeat("representation", 100))
				}), mws...)
				for range 2 {
					r := httptest.NewRequest("GET", "/", nil)
					r.Header.Set("Accept-Encoding", "gzip")
					if got := runRequest(t, h, r); got.Code != 200 || got.Body.Len() == 0 {
						t.Fatalf("middleware delivery changed: %d, %d bytes", got.Code, got.Body.Len())
					}
				}
				if calls != 2 {
					t.Fatalf("middleware revived stale representation: calls=%d", calls)
				}
			})
		}
	}
}

func TestCacheFreshnessBoundarySlowMiddlewareBuffer(t *testing.T) {
	for _, other := range []Middleware{ETag(), Retry(2), Compress(Gzip)} {
		for _, cacheFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%T/cache-first=%t", other, cacheFirst), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					mws := []Middleware{Cache("1h"), other}
					if !cacheFirst {
						mws[0], mws[1] = mws[1], mws[0]
					}
					calls := 0
					h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						calls++
						w.Header().Set("Cache-Control", "max-age=1")
						w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
						w.WriteHeader(http.StatusOK)
						time.Sleep(2 * time.Second)
						_, _ = io.WriteString(w, "body")
					}), mws...)
					for range 2 {
						runRequest(t, h, httptest.NewRequest("GET", "/", nil))
					}
					if calls != 2 {
						t.Fatalf("inner buffering reset origin freshness: calls=%d", calls)
					}
				})
			})
		}
	}
}
