package statute

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"statute.kjanat.dev/internal/docker"
	"statute.kjanat.dev/resolved"
)

func TestTimeoutResolveLimits(t *testing.T) {
	m, err := resolveMiddleware(Timeout("1s"))
	if err != nil || m.TimeoutMaxInFlight != 128 || m.MaxResponseBodyBytes != 8<<20 || m.ResponseBufferBudgetBytes != 64<<20 {
		t.Fatalf("defaults=%+v err=%v", m, err)
	}
	for _, mw := range []Middleware{Timeout("1s").MaxInFlight(-1), Timeout("1s").MaxResponseBody("0"), Timeout("1s").BufferBudget("-1"), Timeout("1s").MaxResponseBody("2KiB").BufferBudget("1KiB")} {
		if _, err := resolveMiddleware(mw); err == nil {
			t.Fatal("invalid Timeout limits accepted")
		}
	}
}

func TestTimeoutExportAndDockerLimits(t *testing.T) {
	mw := Timeout("1s").MaxResponseBody("1KiB").BufferBudget("2KiB").MaxInFlight(3)
	cfg := Config{Listeners: Listeners{HTTP(":8080")}, Routes: Routes{Match("/*").Handle(http.NotFoundHandler()).With(mw)}, FallbackRoutes: Routes{Match("/*").Handle(http.NotFoundHandler()).With(mw)}}
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
	dc, err := resolveDocker(Docker().Middleware("bounded", mw))
	if err != nil {
		t.Fatal(err)
	}
	p := &dockerProvider{cfg: dc}
	chain, warning := p.routeMiddleware(&docker.Service{Name: "app"}, docker.Matcher{Middlewares: []string{"bounded"}}, nil)
	if warning != "" || len(chain) != 1 {
		t.Fatalf("chain=%+v warning=%q", chain, warning)
	}
	for _, m := range []resolved.Middleware{exported.Routes[0].Middleware[0], exported.FallbackRoutes[0].Middleware[0], chain[0]} {
		assertTimeoutConfigLimits(t, m)
	}
	native, err := routeHints("native", docker.MiddlewareHints{Timeout: "1s"})
	if err != nil || len(native) != 1 || native[0].TimeoutMaxInFlight != 128 {
		t.Fatalf("native limits=%+v err=%v", native, err)
	}
}

func assertTimeoutConfigLimits(t *testing.T, m resolved.Middleware) {
	t.Helper()
	if m.TimeoutMaxInFlight != 3 || m.MaxResponseBodyBytes != 1024 || m.ResponseBufferBudgetBytes != 2048 {
		t.Fatalf("normalized limits lost: %+v", m)
	}
}
