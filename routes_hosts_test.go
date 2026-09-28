package statute

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"statute.kjanat.dev/resolved"
)

func hostsConfig(routes ...*Route) Config {
	return Config{
		Listeners: Listeners{HTTP(":8080")},
		Upstreams: Upstreams{"app": Pool{Backends: []Backend{{Address: "http://127.0.0.1:9000"}}}},
		Routes:    routes,
	}
}

func TestHostsCopiesAppendsAndPreservesOrder(t *testing.T) {
	t.Parallel()
	input := []string{"App.Example.COM.", "www.example.com"}
	route := Match("/app/*").Hosts(input...).Hosts().Hosts("third.example").ProxyTo("app")
	input[0] = "changed.example"
	cfg := hostsConfig(Match("/before").Serve("./before"), route, Match("/*").Serve("./after"))
	want := []string{"", "App.Example.COM.", "www.example.com", "third.example", ""}
	first := mustResolve(t, cfg)
	got := make([]string, 0, len(first.Routes))
	for _, concrete := range first.Routes {
		got = append(got, concrete.Host)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("host order/spelling: got %v, want %v", got, want)
	}
	if route.host != "" || route.hostSet {
		t.Fatal("Resolve mutated the surface declaration's single-host state")
	}
	if second := mustResolve(t, cfg); !reflect.DeepEqual(first, second) {
		t.Fatal("resolving the same declaration twice changed the result")
	}
	resolvedAfterEmptyCall := mustResolve(t, hostsConfig(Match("/*").Hosts().Hosts("a.example").Serve("./public")))
	if resolvedAfterEmptyCall.Routes[0].Host != "a.example" {
		t.Fatal("empty append before a nonempty final collection changed the host")
	}
}

func TestHostsRejectsAmbiguousDeclarations(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		route *Route
		want  string
	}{
		{"empty final list", Match("/bad/*").Hosts(), "at least one host"},
		{"empty entry", Match("/bad/*").Hosts("a.example", ""), `host[1] ""`},
		{"duplicate", Match("/bad/*").Hosts("a.example", "a.example"), `host[1] "a.example" duplicates host[0]`},
		{"case duplicate across calls", Match("/bad/*").Hosts("a.example").Hosts("A.EXAMPLE"), `host[1] "A.EXAMPLE" duplicates host[0]`},
		{"unicode fold duplicate", Match("/bad/*").Hosts("Σ.example", "ς.example"), `host[1] "ς.example" duplicates host[0]`},
		{"Host before Hosts", Match("/bad/*").Host("a.example").Hosts("b.example"), "cannot combine Host and Hosts"},
		{"Host after Hosts", Match("/bad/*").Hosts("a.example").Host("b.example"), "cannot combine Host and Hosts"},
		{"empty Host before Hosts", Match("/bad/*").Host("").Hosts("a.example"), "cannot combine Host and Hosts"},
		{"empty Host after Hosts", Match("/bad/*").Hosts("a.example").Host(""), "cannot combine Host and Hosts"},
		{"cleared Host still explicit", Match("/bad/*").Host("old.example").Host("").Hosts("a.example"), "cannot combine Host and Hosts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := hostsConfig(Match("/before").Hosts("one.example", "two.example").Serve("./before"), tc.route.Serve("./public"))
			_, err := Resolve(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), `route[1] "/bad/*"`) {
				t.Fatalf("error=%v, want original declaration index and %q", err, tc.want)
			}
		})
	}
}

func TestHostsAllActionsMatchManualExpansion(t *testing.T) {
	t.Parallel()
	handler := http.NewServeMux()
	for _, action := range []struct {
		name  string
		apply func(*Route) *Route
	}{
		{"proxy", func(r *Route) *Route { return r.ProxyTo("app") }},
		{"serve", func(r *Route) *Route { return r.Serve("./public") }},
		{"redirect", func(r *Route) *Route {
			return r.RedirectTo("https://target.example{request_uri}", http.StatusTemporaryRedirect)
		}},
		{"handler", func(r *Route) *Route { return r.Handle(handler) }},
	} {
		t.Run(action.name, func(t *testing.T) {
			base := func() *Route {
				return Match("/app/*").ClientIPs("192.0.2.1/24").With(SetRequestHeader("X-Route", "app"), Retry(2, OnStatus(503)))
			}
			expanded := mustResolve(t, hostsConfig(action.apply(base().Hosts("a.example", "b.example"))))
			manual := mustResolve(t, hostsConfig(action.apply(base().Host("a.example")), action.apply(base().Host("b.example"))))
			if !reflect.DeepEqual(expanded, manual) {
				t.Fatal("Hosts differs from ordinary Go expansion")
			}
			assertHostsIntentionalReferences(t, expanded, handler)
		})
	}
}

func assertHostsIntentionalReferences(t *testing.T, cfg *resolved.Config, handler http.Handler) {
	t.Helper()
	for _, route := range cfg.Routes {
		if route.Upstream != nil && route.Upstream != cfg.Upstreams["app"] {
			t.Fatal("expansion cloned the shared upstream pool")
		}
		if route.HandlerRoute && route.Handler != handler {
			t.Fatal("expansion replaced the opaque handler")
		}
	}
}

func TestHostsPreservesExistingHostValues(t *testing.T) {
	t.Parallel()
	values := []string{"*.example.com", "example.com:443", "bücher.example", "space value", "Example.COM.", "example.com"}
	multi := mustResolve(t, hostsConfig(Match("/*").Hosts(values...).Serve("./public")))
	for i, host := range values {
		legacy := mustResolve(t, hostsConfig(Match("/*").Host(host).Serve("./public")))
		if !reflect.DeepEqual(multi.Routes[i], legacy.Routes[0]) {
			t.Errorf("Host semantics changed for %q", host)
		}
	}
}

func TestHostsLeavesLegacySingleHostBytesUnchanged(t *testing.T) {
	t.Parallel()
	for _, host := range []string{"", "a.example", "A.Example.", "*.example"} {
		legacy := Match("/*").Serve("./public")
		legacy.host = host
		declared := Match("/*").Host("discarded.example").Host(host).Serve("./public")
		var oldJSON, newJSON bytes.Buffer
		if err := Export(hostsConfig(legacy), &oldJSON); err != nil {
			t.Fatal(err)
		}
		if err := Export(hostsConfig(declared), &newJSON); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(oldJSON.Bytes(), newJSON.Bytes()) {
			t.Fatalf("legacy Host(%q) serialized differently", host)
		}
	}
}

func TestHostsResolvedMutableValuesAreIndependent(t *testing.T) {
	t.Parallel()
	declaration := Match("/*").Hosts("a.example", "b.example").ClientIPs("192.0.2.0/24").RedirectTo("https://target.example", http.StatusFound).With(
		Retry(2, OnStatus(503)), Compress(Gzip),
		CORS().Origins("https://client.example").Methods("GET").Headers("X-Allowed").ExposeHeaders("X-Exposed"),
		BasicAuth("private", map[string]string{"alice": hunter2Hash}), AllowIPs("192.0.2.0/24"),
	)
	cfg := hostsConfig(declaration)
	result := mustResolve(t, cfg)
	before := resolvedRouteJSON(t, result.Routes[1])
	first := result.Routes[0]
	first.ClientIPCIDRs[0] = "203.0.113.0/24"
	first.Redirect.Target = "https://changed.example"
	first.Middleware[0].RetryOnStatuses[0] = 502
	first.Middleware[1].CompressAlgos[0] = resolved.Brotli
	first.Middleware[2].CORSOrigins[0] = "https://changed.example"
	first.Middleware[2].CORSMethods[0] = "POST"
	first.Middleware[2].CORSHeaders[0] = "X-Changed"
	first.Middleware[2].CORSExposeHeaders[0] = "X-Changed"
	first.Middleware[3].BasicAuthUsers["alice"] = "changed"
	first.Middleware[4].IPCIDRs[0] = "203.0.113.0/24"
	first.Middleware[0].RetryMax = 99
	if !bytes.Equal(before, resolvedRouteJSON(t, result.Routes[1])) {
		t.Fatal("mutating one concrete route changed its sibling")
	}
	fresh := mustResolve(t, cfg)
	if !bytes.Equal(before, resolvedRouteJSON(t, fresh.Routes[1])) {
		t.Fatal("resolved mutation reached the reusable surface declaration")
	}
}

func resolvedRouteJSON(t *testing.T, route *resolved.Route) []byte {
	t.Helper()
	data, err := json.Marshal(route)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestHostsErrorsKeepHostEntryAndNilRouteSafe(t *testing.T) {
	t.Parallel()
	_, err := Resolve(hostsConfig(Match("/*").Hosts("a.example", "b.example").ProxyTo("missing")))
	if err == nil || !strings.Contains(err.Error(), `route[0] "/*": host[0] "a.example": unknown upstream "missing"`) {
		t.Fatalf("expanded action error lacks source context: %v", err)
	}
	_, err = Resolve(hostsConfig(nil))
	if err == nil || !strings.Contains(err.Error(), "route[0]: nil route") {
		t.Fatalf("nil declaration error: %v", err)
	}
}
