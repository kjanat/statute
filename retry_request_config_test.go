package statute

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"statute.kjanat.dev/internal/docker"
	"statute.kjanat.dev/resolved"
)

func TestRetryRequestBudgetConfiguration(t *testing.T) {
	for _, tc := range []struct {
		size string
		want int64
	}{{"", 64 << 20}, {"1B", 1}, {"128KiB", 128 << 10}} {
		m, err := resolveMiddleware(Retry(2).RequestBufferBudget(tc.size))
		if err != nil {
			t.Fatal(err)
		}
		if m.RequestBufferBudgetBytes != tc.want || m.ResponseBufferBudgetBytes != 64<<20 || m.MaxResponseBodyBytes != 8<<20 {
			t.Fatalf("size=%q limits=%+v", tc.size, m)
		}
	}
	for _, size := range []string{"0", "-1", "invalid", "999999999999999999999GiB"} {
		if _, err := resolveMiddleware(Retry(2).RequestBufferBudget(size)); err == nil {
			t.Fatalf("invalid request budget accepted: %q", size)
		}
		if _, err := resolveDocker(Docker().Middleware("retry", Retry(2).RequestBufferBudget(size))); err == nil {
			t.Fatalf("invalid Docker request budget accepted: %q", size)
		}
	}
}

func TestRetryRequestBudgetExport(t *testing.T) {
	cfg := Config{
		Listeners:      Listeners{HTTP(":8080")},
		Routes:         Routes{Match("/*").Handle(http.NotFoundHandler()).With(Retry(2).RequestBufferBudget("128KiB"))},
		FallbackRoutes: Routes{Match("/*").Handle(http.NotFoundHandler()).With(Retry(2).RequestBufferBudget("1B"))},
	}
	var encoded bytes.Buffer
	if err := Export(cfg, &encoded); err != nil {
		t.Fatal(err)
	}
	var exported struct {
		Routes, FallbackRoutes []struct{ Middleware []resolved.Middleware }
	}
	if err := json.Unmarshal(encoded.Bytes(), &exported); err != nil {
		t.Fatal(err)
	}
	if exported.Routes[0].Middleware[0].RequestBufferBudgetBytes != 128<<10 || exported.FallbackRoutes[0].Middleware[0].RequestBufferBudgetBytes != 1 {
		t.Fatalf("export lost request budgets: %s", encoded.String())
	}
}

func TestRetryRequestBudgetDockerAssembly(t *testing.T) {
	cfg, err := resolveDocker(Docker().DefaultMiddleware(Retry(2).RequestBufferBudget("128KiB")).
		Middleware("retry", Retry(2).RequestBufferBudget("1B")))
	if err != nil {
		t.Fatal(err)
	}
	p := &dockerProvider{cfg: cfg}
	mws, warning := p.routeMiddleware(&docker.Service{Name: "app"}, docker.Matcher{Middlewares: []string{"retry"}}, nil)
	if warning != "" || len(mws) != 2 {
		t.Fatalf("middleware=%+v warning=%q", mws, warning)
	}
	if mws[0].RequestBufferBudgetBytes != 128<<10 || mws[1].RequestBufferBudgetBytes != 1 {
		t.Fatalf("assembled request budgets=%+v", mws)
	}
}
