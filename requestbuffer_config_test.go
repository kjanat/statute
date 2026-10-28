package statute

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"statute.kjanat.dev/internal/docker"
	"statute.kjanat.dev/resolved"
)

func TestRequestBufferConfigurationAndTools(t *testing.T) {
	cfg := Config{
		Listeners:      Listeners{HTTP(":8080")},
		RequestBuffers: RequestBufferResources{MemoryBudget: "64KiB", Storage: "/operator/spool", DiskBudget: "4MiB"},
		Routes:         Routes{Match("/upload").Handle(http.NotFoundHandler()).With(RequestBuffer("5MiB").MemoryThreshold("16KiB"))},
		FallbackRoutes: Routes{Match("/*").Handle(http.NotFoundHandler()).With(RequestBuffer("1KiB"))},
		Docker:         Docker().Middleware("buffer", RequestBuffer("2MiB")),
	}
	var out bytes.Buffer
	if err := Export(cfg, &out); err != nil {
		t.Fatal(err)
	}
	var r resolved.Config
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if r.RequestBuffers.MemoryBudgetBytes != 64<<10 || r.RequestBuffers.DiskBudgetBytes != 4<<20 || r.RequestBuffers.Storage != "/operator/spool" {
		t.Fatalf("resources=%+v", r.RequestBuffers)
	}
	if r.Routes[0].Middleware[0].RequestBufferMaxBytes != 5<<20 || r.FallbackRoutes[0].Middleware[0].RequestBufferMemoryThresholdBytes != 1024 {
		t.Fatal("route policy lost")
	}
	assertRequestBufferDockerPolicy(t, r.Docker)
	assertRequestBufferTools(t, cfg)
}

func assertRequestBufferDockerPolicy(t *testing.T, cfg *resolved.Docker) {
	t.Helper()
	provider := &dockerProvider{cfg: cfg}
	chain, warning := provider.routeMiddleware(&docker.Service{Name: "app"}, docker.Matcher{Middlewares: []string{"buffer"}}, nil)
	if warning != "" || len(chain) != 1 || chain[0].RequestBufferMaxBytes != 2<<20 {
		t.Fatalf("Docker chain=%v warning=%q", chain, warning)
	}
}

func assertRequestBufferTools(t *testing.T, cfg Config) {
	t.Helper()
	var out bytes.Buffer
	out.Reset()
	if err := GraphDOT(cfg, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"memory=65536", "disk=4194304", "/operator/spool", "max=5242880 threshold=16384", "fallback_routes", "docker.middleware"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("graph missing %q", want)
		}
	}
	findings, err := Lint(cfg)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, f := range findings {
		if f.Code == "RBF001" {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("resource lint findings=%d", found)
	}
}

func TestRequestBufferInvalidConfiguration(t *testing.T) {
	for _, c := range []RequestBufferResources{
		{MemoryBudget: "0"},
		{MemoryBudget: "-1"},
		{Storage: "/tmp"},
		{DiskBudget: "1MiB"},
		{Storage: "relative", DiskBudget: "1MiB"},
		{Storage: "/tmp", DiskBudget: "0"},
	} {
		if _, err := Resolve(Config{RequestBuffers: c}); err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
	for _, mw := range []Middleware{RequestBuffer("0"), RequestBuffer("-1"), RequestBuffer("bad"), RequestBuffer("1B").MemoryThreshold("2B"), RequestBuffer("1B").MemoryThreshold("0")} {
		if _, err := resolveMiddleware(mw); err == nil {
			t.Errorf("accepted %+v", mw)
		}
		if _, err := resolveDocker(Docker().Middleware("buffer", mw)); err == nil {
			t.Error("Docker accepted invalid policy")
		}
	}
	c, err := resolveRequestBufferResources(RequestBufferResources{})
	if err != nil || c.MemoryBudgetBytes != 64<<20 || c.DiskBudgetBytes != 0 || c.Storage != "" {
		t.Fatalf("defaults=%+v %v", c, err)
	}
}
