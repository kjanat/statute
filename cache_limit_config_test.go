package statute

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"statute.kjanat.dev/internal/docker"
	"statute.kjanat.dev/resolved"
)

func TestCacheLimitDefaults(t *testing.T) {
	t.Parallel()
	m, err := resolveMiddleware(Cache("1h"))
	if err != nil {
		t.Fatal(err)
	}
	if m.CacheMaxEntries != 1024 || m.MaxResponseBodyBytes != 8<<20 || m.ResponseBufferBudgetBytes != 64<<20 {
		t.Fatalf("defaults=%+v", m)
	}
}

func TestCacheLimitInvalidConfiguration(t *testing.T) {
	t.Parallel()
	for _, mw := range []Middleware{
		Cache("1h").MaxEntries(-1), Cache("1h").MaxResponseBody("0"),
		Cache("1h").MaxResponseBody("-1"), Cache("1h").MaxResponseBody("invalid"),
		Cache("1h").BufferBudget("0"), Cache("1h").BufferBudget("-1"),
		Cache("1h").BufferBudget("invalid"), Cache("1h").BufferBudget("9999999999999999999999GiB"),
		Cache("1h").MaxResponseBody("8MiB").BufferBudget("4MiB"),
	} {
		if _, err := resolveMiddleware(mw); err == nil {
			t.Fatalf("invalid config accepted: %+v", mw)
		}
		if _, err := resolveDocker(Docker().Middleware("bad-cache", mw)); err == nil {
			t.Fatal("invalid Docker config accepted")
		}
	}
}

func TestCacheLimitExportAndDocker(t *testing.T) {
	t.Parallel()
	cache := Cache("1h").MaxEntries(7).MaxResponseBody("16KiB").BufferBudget("2MiB")
	cfg := Config{Listeners: Listeners{HTTP(":0")},
		Routes:         Routes{Match("/*").Handle(http.NotFoundHandler()).With(cache)},
		FallbackRoutes: Routes{Match("/*").Handle(http.NotFoundHandler()).With(cache)},
	}
	var output bytes.Buffer
	if err := Export(cfg, &output); err != nil {
		t.Fatal(err)
	}
	var exported struct {
		Routes, FallbackRoutes []struct{ Middleware []resolved.Middleware }
	}
	if err := json.Unmarshal(output.Bytes(), &exported); err != nil {
		t.Fatal(err)
	}
	assertCacheLimits(t, exported.Routes[0].Middleware[0])
	assertCacheLimits(t, exported.FallbackRoutes[0].Middleware[0])
	dcfg, err := resolveDocker(Docker().DefaultMiddleware(cache).Middleware("bounded", cache))
	if err != nil {
		t.Fatal(err)
	}
	p := &dockerProvider{cfg: dcfg}
	mws, warning := p.routeMiddleware(&docker.Service{Name: "app"}, docker.Matcher{Middlewares: []string{"bounded"}}, nil)
	if warning != "" || len(mws) != 2 {
		t.Fatalf("middleware=%+v warning=%s", mws, warning)
	}
	for _, mw := range mws {
		assertCacheLimits(t, mw)
	}
}

func assertCacheLimits(t *testing.T, m resolved.Middleware) {
	t.Helper()
	if m.CacheMaxEntries != 7 || m.MaxResponseBodyBytes != 16<<10 || m.ResponseBufferBudgetBytes != 2<<20 {
		t.Fatalf("limits lost: %+v", m)
	}
}

func TestCacheBodyLimitExactBoundary(t *testing.T) {
	for _, size := range []int{16, 17} {
		calls := 0
		body := strings.Repeat("x", size)
		h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			_, _ = io.WriteString(w, body[:8])
			_, _ = io.WriteString(w, body[8:])
		}), Cache("1h").MaxResponseBody("16").BufferBudget("2MiB"))
		for range 2 {
			rec := runRequest(t, h, httptest.NewRequest("GET", "/", nil))
			if rec.Code != 200 || rec.Body.String() != body {
				t.Fatalf("size=%d: %d %q", size, rec.Code, rec.Body.String())
			}
		}
		want := 1
		if size > 16 {
			want = 2
		}
		if calls != want {
			t.Fatalf("size=%d calls=%d want=%d", size, calls, want)
		}
	}
}
