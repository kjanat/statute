package statute

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"statute.kjanat.dev/internal/docker"
)

func TestResolveCacheRequestIDOrder(t *testing.T) {
	t.Parallel()
	for _, rid := range []Middleware{RequestID(), RequestID().Header("X-Identity").From("X-User"), RequestID().Header("Authorization")} {
		for _, fallback := range []bool{false, true} {
			cfg := Config{Listeners: Listeners{HTTP(":8080")}, Routes: Routes{
				Match("/*").Handle(http.NotFoundHandler()).With(Cache("1h"), ETag(), Retry(2), rid),
			}}
			if fallback {
				cfg.FallbackRoutes, cfg.Routes = cfg.Routes, nil
			}
			_, err := Resolve(cfg)
			if err == nil || !strings.Contains(err.Error(), "RequestID must precede Cache") {
				t.Fatalf("fallback=%v: %v", fallback, err)
			}
		}
	}
}

func TestCacheRequestIDSafeOrder(t *testing.T) {
	t.Parallel()
	mws, err := resolveMiddlewares([]Middleware{Cache("0s"), RequestID().Header("X-Identity").From("X-User"), ETag(), Cache("1h"), Retry(2), Cache("1h")})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	h := wrapMiddleware(mws, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Vary", "X-Identity")
		switch r.Header.Get("X-Identity") {
		case "alice":
			w.Header().Set("X-Account", "alice")
		case "bob":
			w.Header().Set("X-Account", "bob")
		default:
			t.Error("identity missing")
		}
	}))
	for _, id := range []string{"alice", "bob", "alice", "bob"} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("X-User", id)
		rec := runRequest(t, h, r)
		if rec.Header().Get("X-Account") != id || rec.Header().Get("X-Identity") != id {
			t.Fatalf("%s: %v", id, rec.Header())
		}
	}
	if calls != 2 {
		t.Fatalf("calls=%d want=2", calls)
	}
}

func TestCacheRequestIDFreshOnHits(t *testing.T) {
	t.Parallel()
	mws, err := resolveMiddlewares([]Middleware{RequestID(), Cache("1h")})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	h := wrapMiddleware(mws, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { calls++ }))
	previous := ""
	for range 2 {
		r, holder := installRIDHolder(httptest.NewRequest("GET", "/", nil))
		rec := runRequest(t, h, r)
		id := rec.Header().Get(defaultRequestIDHeader)
		if id == "" || id == previous || id != holder.id {
			t.Fatalf("id=%q previous=%q holder=%q", id, previous, holder.id)
		}
		previous = id
	}
	if calls != 1 {
		t.Fatalf("public cache not retained: %d", calls)
	}
}

func TestDockerCacheRequestIDOrder(t *testing.T) {
	t.Parallel()
	cfg, err := resolveDocker(Docker().Middleware("cache", Cache("1h")).Middleware("identity", RequestID().Header("X-Identity").From("X-Origin")))
	if err != nil {
		t.Fatal(err)
	}
	p := &dockerProvider{cfg: cfg}
	_, warning := p.routeMiddleware(&docker.Service{Name: "app"}, docker.Matcher{Middlewares: []string{"cache", "identity"}}, nil)
	if !strings.Contains(warning, "RequestID must precede Cache") {
		t.Fatalf("unsafe combined chain: %q", warning)
	}
	if _, warning = p.routeMiddleware(&docker.Service{Name: "app"}, docker.Matcher{Middlewares: []string{"identity", "cache"}}, nil); warning != "" {
		t.Fatal(warning)
	}
}

func TestCacheRateLimitPlacement(t *testing.T) {
	t.Parallel()
	for _, outside := range []bool{true, false} {
		mws := []Middleware{Cache("1h"), RateLimit("1/h")}
		if outside {
			mws[0], mws[1] = mws[1], mws[0]
		}
		resolvedMWs, err := resolveMiddlewares(mws)
		if err != nil {
			t.Fatal(err)
		}
		calls := 0
		h := wrapMiddleware(resolvedMWs, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { calls++ }))
		for i := range 3 {
			want := http.StatusOK
			if outside && i > 0 {
				want = http.StatusTooManyRequests
			}
			if rec := runRequest(t, h, httptest.NewRequest("GET", "/", nil)); rec.Code != want {
				t.Fatalf("outside=%v request=%d status=%d want=%d", outside, i, rec.Code, want)
			}
		}
		if calls != 1 {
			t.Fatalf("outside=%v calls=%d", outside, calls)
		}
	}
}
