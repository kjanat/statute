package statute

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"statute.kjanat.dev/resolved"
)

func TestFallbackRoutesExport(t *testing.T) {
	t.Parallel()
	for _, withHandler := range []bool{false, true} {
		t.Run(map[bool]string{false: "routes only", true: "routes and handler"}[withHandler], func(t *testing.T) {
			t.Parallel()
			cfg := Config{
				Listeners: Listeners{HTTP(":0")},
				Upstreams: Upstreams{"shared": Pool{Backends: []Backend{{Address: "https://assets.example.net"}}}},
				Routes:    Routes{Match("/api/*").ProxyTo("shared")},
				FallbackRoutes: Routes{
					Match("/favicon.ico").Hosts("one.example", "two.example").ProxyTo("shared").With(ReplacePath("/site/favicon.ico")),
					Match("/*").Handle(noContentHandler),
				},
			}
			if withHandler {
				cfg.Fallback = http.NotFoundHandler()
			}
			var output bytes.Buffer
			if err := Export(cfg, &output); err != nil {
				t.Fatal(err)
			}
			var exported resolved.Config
			if err := json.Unmarshal(output.Bytes(), &exported); err != nil {
				t.Fatal(err)
			}
			if len(exported.Routes) != 1 || len(exported.FallbackRoutes) != 3 {
				t.Fatalf("route tables were merged or not expanded: %s", output.String())
			}
			if exported.HasFallback != withHandler {
				t.Errorf("HasFallback = %v, want application handler marker %v", exported.HasFallback, withHandler)
			}
			for i, host := range []string{"one.example", "two.example"} {
				assertExportedFallbackRoute(t, exported.FallbackRoutes[i], host)
			}
			if len(exported.Upstreams) != 1 {
				t.Errorf("shared pool duplicated in export: %+v", exported.Upstreams)
			}
		})
	}
}

func assertExportedFallbackRoute(t *testing.T, route *resolved.Route, host string) {
	t.Helper()
	if route.Host != host || route.Pattern != "/favicon.ico" || route.Upstream == nil || route.Upstream.Name != "shared" || len(route.Middleware) != 1 {
		t.Errorf("terminal route for %q lost normalized declaration: %+v", host, route)
	}
}

func TestFallbackRoutesGraph(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Listeners: Listeners{
			HTTP(":80").RedirectTo("https"),
			HTTPS(":443", StaticTLS("cert.pem", "key.pem")),
		},
		Upstreams: Upstreams{"shared": Pool{Backends: []Backend{{Address: "https://assets.example.net"}}}},
		Routes:    Routes{Match("/api/*").ProxyTo("shared")},
		FallbackRoutes: Routes{
			Match("/favicon.ico").ProxyTo("shared"),
			Match("/*").Host("legacy.example").ProxyTo("shared"),
		},
		Fallback: http.NotFoundHandler(),
	}
	var output bytes.Buffer
	if err := GraphDOT(cfg, &output); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`FR0 [shape=box`, `terminal #1: * /favicon.ico`, `terminal #2: legacy.example /*`,
		`L1 -> FR0 [label="static and Docker miss", style=dashed]`,
		`FR0 -> FR1 [label="no match", style=dashed]`,
		`FR1 -> F [label="no match", style=dashed]`,
		`R0 -> P_shared;`, `FR0 -> P_shared;`, `FR1 -> P_shared;`,
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("graph missing %q: %s", want, output.String())
		}
	}
	for _, forbidden := range []string{`L0 -> FR`, `L1 -> F [`, `L1 -> FR1`} {
		if strings.Contains(output.String(), forbidden) {
			t.Errorf("graph bypasses terminal ordering with %q: %s", forbidden, output.String())
		}
	}
	if got := strings.Count(output.String(), `P_shared [shape=ellipse`); got != 1 {
		t.Errorf("pool node count = %d, want one", got)
	}
}

func TestFallbackRoutesMiddlewareLint(t *testing.T) {
	t.Parallel()
	for _, secure := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "https"}[secure], func(t *testing.T) {
			t.Parallel()
			route := func() *Route {
				return Match("/*").Host("app.example").Handle(noContentHandler).With(
					RateLimit("30/min"),
					BasicAuth("realm", map[string]string{"alice": "$2a$10$HwrzUQtDrRX0/09su3BahezCIqD.f4HjCkYD5b9w8gl4eUkPJzCyu"}),
				)
			}
			cfg := Config{Listeners: Listeners{HTTP(":80")}, Routes: Routes{route()}, FallbackRoutes: Routes{route()}}
			if secure {
				cfg.Listeners = Listeners{HTTPS(":443", StaticTLS("cert.pem", "key.pem"))}
			}
			findings, err := Lint(cfg)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, finding := range findings {
				if finding.Code == "RL001" || finding.Code == "AUTH001" {
					got = append(got, finding.Code+":"+finding.Path)
				}
			}
			want := []string{"RL001:routes[0].middleware[0]", "RL001:fallback_routes[0].middleware[0]"}
			if !secure {
				want = append(want, "AUTH001:routes[0].middleware[1]", "AUTH001:fallback_routes[0].middleware[1]")
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("middleware diagnostics = %v, want %v", got, want)
			}
		})
	}
}

func TestFallbackRoutesShadowLint(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		route *Route
		want  int
	}{
		{"ordinary catchall", Match("/*").Handle(noContentHandler), 1},
		{"host constrained", Match("/*").Host("app.example").Handle(noContentHandler), 0},
		{"path constrained", Match("/api/*").Handle(noContentHandler), 0},
		{"client constrained", Match("/*").ClientIPs("192.0.2.0/24").Handle(noContentHandler), 0},
		{"terminal catchall only", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := Config{Listeners: Listeners{HTTP(":0")}, FallbackRoutes: Routes{Match("/*").Handle(noContentHandler)}}
			if tc.route != nil {
				cfg.Routes = Routes{tc.route}
			}
			findings, err := Lint(cfg)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, finding := range findings {
				if finding.Code != "FB001" {
					continue
				}
				count++
				if finding.Path != "routes[0]" || !strings.Contains(finding.Message, "terminal fallback routes") || !strings.Contains(finding.Message, "Config.FallbackRoutes") {
					t.Errorf("imprecise shadow finding: %+v", finding)
				}
			}
			if count != tc.want {
				t.Errorf("FB001 count = %d, want %d", count, tc.want)
			}
		})
	}
}
