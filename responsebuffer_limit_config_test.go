package statute

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"statute.kjanat.dev/internal/docker"
	"statute.kjanat.dev/resolved"
)

func TestResolveResponseBufferLimits(t *testing.T) {
	for name, mw := range limitedRenderMiddlewares("", "") {
		t.Run(name, func(t *testing.T) {
			m, err := resolveMiddleware(mw)
			if err != nil {
				t.Fatal(err)
			}
			if m.MaxResponseBodyBytes != 8<<20 || m.ResponseBufferBudgetBytes != 64<<20 {
				t.Fatalf("defaults=%+v", m)
			}
		})
	}
}

func TestResolveResponseBufferInvalidLimits(t *testing.T) {
	for _, size := range []string{"0", "-1", "invalid", "99999999999999999999999GiB"} {
		for name, mw := range limitedRenderMiddlewares(size, "64MiB") {
			t.Run(name+"/"+size, func(t *testing.T) {
				if _, err := resolveMiddleware(mw); err == nil {
					t.Fatal("invalid body limit accepted")
				}
			})
		}
		for name, mw := range limitedRenderMiddlewares("8MiB", size) {
			t.Run(name+"/budget/"+size, func(t *testing.T) {
				if _, err := resolveMiddleware(mw); err == nil {
					t.Fatal("invalid budget accepted")
				}
			})
		}
	}
	for _, mw := range limitedRenderMiddlewares("8MiB", "4MiB") {
		if _, err := resolveMiddleware(mw); err == nil {
			t.Fatal("budget smaller than body limit accepted")
		}
	}
}

func TestExportResponseBufferLimits(t *testing.T) {
	cfg := Config{Listeners: Listeners{HTTP(":8080")},
		Routes:         Routes{Match("/*").Handle(http.NotFoundHandler()).With(ETag().MaxResponseBody("16KiB").BufferBudget("128KiB"))},
		FallbackRoutes: Routes{Match("/*").Handle(http.NotFoundHandler()).With(Retry(2).MaxResponseBody("16KiB").BufferBudget("128KiB"))},
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
	for _, m := range []resolved.Middleware{exported.Routes[0].Middleware[0], exported.FallbackRoutes[0].Middleware[0]} {
		if m.MaxResponseBodyBytes != 16<<10 || m.ResponseBufferBudgetBytes != 128<<10 {
			t.Fatalf("export lost limits: %+v", m)
		}
	}
}

func TestDockerResponseBufferLimits(t *testing.T) {
	cfg, err := resolveDocker(Docker().DefaultMiddleware(ETag().MaxResponseBody("16KiB").BufferBudget("128KiB")).
		Middleware("retry", Retry(2).MaxResponseBody("32KiB").BufferBudget("256KiB")))
	if err != nil {
		t.Fatal(err)
	}
	p := &dockerProvider{cfg: cfg}
	mws, warning := p.routeMiddleware(&docker.Service{Name: "app"}, docker.Matcher{Middlewares: []string{"retry"}}, nil)
	if warning != "" || len(mws) != 2 {
		t.Fatalf("middleware=%+v warning=%q", mws, warning)
	}
	if mws[0].MaxResponseBodyBytes != 16<<10 || mws[1].MaxResponseBodyBytes != 32<<10 ||
		mws[0].ResponseBufferBudgetBytes != 128<<10 || mws[1].ResponseBufferBudgetBytes != 256<<10 {
		t.Fatalf("assembled limits=%+v", mws)
	}
	if _, err := resolveDocker(Docker().Middleware("invalid", ETag().MaxResponseBody("0"))); err == nil {
		t.Fatal("invalid Docker limit accepted")
	}
}
