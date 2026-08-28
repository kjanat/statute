package statute

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"statute.kjanat.dev/internal/docker"
	"statute.kjanat.dev/resolved"
)

// tombstoneSet renders a generation's tombstones as "host pattern" strings,
// sorted, so a test can compare envelopes without depending on tier order.
func tombstoneSet(t *testing.T, srv *server) []string {
	t.Helper()
	tab := srv.dynamic.Load()
	if tab == nil {
		return nil
	}
	out := make([]string, 0, len(tab.tombstones))
	for _, c := range tab.tombstones {
		if c.route.Upstream != nil || len(c.route.Middleware) > 0 || len(c.clientPrefixes) > 0 {
			t.Errorf("tombstone carries routing state: %+v", c.route)
		}
		pat := c.matcher.Path
		if c.matcher.PathKind == docker.PathByte {
			pat += "*"
		}
		out = append(out, fmt.Sprintf("%s %s", c.matcher.Host, pat))
	}
	sort.Strings(out)
	return out
}

// fallbackServer points a provider's server at a config with a counting
// fallback, so a test can tell a refusal from a fallback invocation.
func fallbackServer(t *testing.T, srv *server, routes Routes) *atomic.Int64 {
	t.Helper()
	calls := new(atomic.Int64)
	srv.cfg = mustResolve(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Routes:    routes,
		Fallback:  countingFallback(calls),
	})
	return calls
}

// Every drop path that discards a declared routing decision leaves an envelope.
func TestDockerTombstoneEnvelopes(t *testing.T) {
	cases := []struct {
		name       string
		cfg        resolved.Docker
		container  fakeDaemonContainer
		want       []string
		wantRoutes int
		why        string
	}{
		{
			name: "unregistered middleware",
			cfg:  resolved.Docker{TraefikLabels: true},
			container: fakeDaemonContainer{
				name: "app-1", ip: "10.0.0.9", port: 3000,
				labels: map[string]string{
					"traefik.enable":                       "true",
					"traefik.http.routers.app.rule":        "Host(`vault.example.com`)",
					"traefik.http.routers.app.middlewares": "ghost@file",
				},
			},
			want: []string{"vault.example.com /*"},
			why:  "the router named an auth chain statute does not have; the envelope is the parsed matcher verbatim",
		}, {
			name: "no exposed port",
			cfg:  resolved.Docker{TraefikLabels: true},
			container: fakeDaemonContainer{
				name: "shop-1", ip: "10.0.0.9",
				labels: map[string]string{
					"traefik.enable":                 "true",
					"traefik.http.routers.shop.rule": "Host(`shop.example.com`)",
				},
			},
			want: []string{"shop.example.com /*"},
			why:  "the label layer cannot build the backend, so it refuses with the exact parsed matcher rather than the coarser rule reading",
		}, {
			name: "no resolvable service",
			cfg:  resolved.Docker{TraefikLabels: true},
			container: fakeDaemonContainer{
				name: "amb-1", ip: "10.0.0.9", port: 3000,
				labels: map[string]string{
					"traefik.enable":                                     "true",
					"traefik.http.routers.amb.rule":                      "Host(`amb.example.com`)",
					"traefik.http.services.one.loadbalancer.server.port": "1",
					"traefik.http.services.two.loadbalancer.server.port": "2",
				},
			},
			want: []string{"amb.example.com /*"},
			why:  "the label layer refuses an ambiguous router-to-service binding, which still discarded a declared route",
		}, {
			name: "container has no usable IP",
			cfg:  resolved.Docker{TraefikLabels: true},
			container: fakeDaemonContainer{
				name: "netless-1", port: 3000,
				labels: map[string]string{
					"traefik.enable":                "true",
					"traefik.http.routers.api.rule": "Host(`a.example.com`) && PathPrefix(`/api`)",
				},
			},
			want: []string{"a.example.com /api*"},
			why:  "a container-level failure must not degrade a perfectly parseable rule into a global refusal",
		}, {
			name: "unsupported matcher keeps its sibling",
			cfg:  resolved.Docker{TraefikLabels: true},
			container: fakeDaemonContainer{
				name: "admin-1", ip: "10.0.0.9", port: 3000,
				labels: map[string]string{
					"traefik.enable":                  "true",
					"traefik.http.routers.admin.rule": "Host(`admin.example.com`) && ClientIP(`10.0.0.0/8`)",
				},
			},
			want: []string{"admin.example.com /*"},
			why:  "dropping the CIDR only adds requests, and it is never rebuilt onto the tombstone",
		}, {
			name: "disjunction widens to global",
			cfg:  resolved.Docker{TraefikLabels: true},
			container: fakeDaemonContainer{
				name: "or-1", ip: "10.0.0.9", port: 3000,
				labels: map[string]string{
					"traefik.enable":               "true",
					"traefik.http.routers.or.rule": "Host(`a.example.com`) || ClientIP(`10.0.0.0/8`)",
				},
			},
			want: []string{" /*"},
			why:  "keeping only the understood branch would hand every other host inside 10/8 to the fallback",
		}, {
			name: "negation keeps its sibling",
			cfg:  resolved.Docker{TraefikLabels: true},
			container: fakeDaemonContainer{
				name: "neg-1", ip: "10.0.0.9", port: 3000,
				labels: map[string]string{
					"traefik.enable":                "true",
					"traefik.http.routers.neg.rule": "Host(`app.example.com`) && !PathPrefix(`/public`)",
				},
			},
			want: []string{"app.example.com /*"},
			why:  "the negation node becomes unconstrained in place instead of poisoning the whole rule",
		}, {
			name: "malformed rule is global",
			cfg:  resolved.Docker{TraefikLabels: true},
			container: fakeDaemonContainer{
				name: "bad-1", ip: "10.0.0.9", port: 3000,
				labels: map[string]string{
					"traefik.enable":                "true",
					"traefik.http.routers.bad.rule": "Host(`a.example.com`) &&",
				},
			},
			want: []string{" /*"},
			why:  "a rule the operator wrote and statute cannot read is intent it cannot bound",
		}, {
			name: "native labels drop",
			cfg:  resolved.Docker{},
			container: fakeDaemonContainer{
				name: "native-1", ip: "10.0.0.9",
				labels: map[string]string{
					"statute.enable": "true",
					"statute.host":   "native.example.com",
					"statute.path":   "/admin/*",
				},
			},
			want: []string{"native.example.com /admin/*"},
			why:  "the statute schema declares routes too, and discarding them has the same consequence",
		}, {
			name: "traefik.enable=false declares nothing",
			cfg:  resolved.Docker{TraefikLabels: true},
			container: fakeDaemonContainer{
				name: "off-1", ip: "10.0.0.9",
				labels: map[string]string{
					"traefik.enable":                "false",
					"traefik.http.routers.off.rule": "Host(`off.example.com`)",
				},
			},
			want: nil,
			why:  "an explicit opt-out must never delete the operator's fallback",
		}, {
			name: "statute.enable=false declares nothing",
			cfg:  resolved.Docker{},
			container: fakeDaemonContainer{
				name: "noff-1", ip: "10.0.0.9",
				labels: map[string]string{
					"statute.enable": "false",
					"statute.host":   "noff.example.com",
				},
			},
			want: nil,
			why:  "the native schema reads an opt-out the same way, and only an unreadable value is a rejection",
		}, {
			name: "exposed by default without route labels",
			cfg:  resolved.Docker{ExposedByDefault: true},
			container: fakeDaemonContainer{
				name: "bare-1", ip: "10.0.0.9",
				labels: map[string]string{},
			},
			want: nil,
			why:  "the catch-all is statute's inference, not a routing decision in the labels",
		}, {
			name: "router with no rule",
			cfg:  resolved.Docker{TraefikLabels: true},
			container: fakeDaemonContainer{
				name: "norule-1", ip: "10.0.0.9", port: 3000,
				labels: map[string]string{
					"traefik.enable":                   "true",
					"traefik.http.routers.norule.rule": "   ",
				},
			},
			want: nil,
			why:  "no rule means no match condition, so the request set is empty and any envelope satisfies the obligation",
		}, {
			name: "unreadable traefik enable label",
			cfg:  resolved.Docker{TraefikLabels: true},
			container: fakeDaemonContainer{
				name: "vault-1", ip: "10.0.0.9", port: 3000,
				labels: map[string]string{
					"traefik.enable":                         "yes",
					"traefik.http.routers.vault.rule":        "Host(`vault.example.com`)",
					"traefik.http.routers.vault.middlewares": "corp-sso",
				},
			},
			want: []string{"vault.example.com /*"},
			why:  "ParseBool rejects yes/no/on/off, so the routes vanish while the rule stays perfectly readable: unreadable intent is a rejection, not an opt-out",
		}, {
			name: "unreadable statute enable label",
			cfg:  resolved.Docker{},
			container: fakeDaemonContainer{
				name: "nvault-1", ip: "10.0.0.9", port: 3000,
				labels: map[string]string{
					"statute.enable": "yes",
					"statute.host":   "nvault.example.com",
				},
			},
			want: []string{"nvault.example.com /*"},
			why:  "the native schema loses the same routes for the same reason",
		}, {
			name: "opted-in catch-all with no usable backend",
			cfg:  resolved.Docker{},
			container: fakeDaemonContainer{
				name: "catchall-1", port: 3000,
				labels: map[string]string{
					"statute.enable": "true",
					"statute.port":   "3000",
				},
			},
			want: []string{" /*"},
			why:  "statute.enable opted this container in to terminating every request, so dropping it hands every request to the fallback: the widest under-refusal the tier can have",
		}, {
			name: "pool config the resolver rejects",
			cfg:  resolved.Docker{},
			container: fakeDaemonContainer{
				name: "hc-1", ip: "10.0.0.9", port: 3000,
				labels: map[string]string{
					"statute.enable":               "true",
					"statute.host":                 "hc.example.com",
					"statute.healthcheck.path":     "/health",
					"statute.healthcheck.interval": "banana",
				},
			},
			want: []string{"hc.example.com /*"},
			why:  "a label typo the provider's resolver rejects drops the whole service, and that is a provider-side refusal the label layer never sees",
		}, {
			name: "backend address no pool handler can be built from",
			cfg:  resolved.Docker{TraefikLabels: true},
			container: fakeDaemonContainer{
				// A space survives resolvePool, which only rejects an
				// empty address, and fails url.Parse in newPoolHandler.
				name: "badaddr-1", ip: "10.0.0.9 ", port: 3000,
				labels: map[string]string{
					"traefik.enable":                    "true",
					"traefik.http.routers.badaddr.rule": "Host(`badaddr.example.com`) && PathPrefix(`/api`)",
				},
			},
			want: []string{"badaddr.example.com /api*"},
			why:  "the pool handler is the last thing that can fail, and its failure discards routes that were otherwise ready to serve",
		}, {
			name: "sibling router keeps serving through a per-route refusal",
			cfg:  resolved.Docker{TraefikLabels: true},
			container: fakeDaemonContainer{
				name: "mixed-1", ip: "10.0.0.9", port: 3000,
				labels: map[string]string{
					"traefik.enable":                        "true",
					"traefik.http.routers.good.rule":        "Host(`good.example.com`)",
					"traefik.http.routers.gone.rule":        "Host(`gone.example.com`)",
					"traefik.http.routers.gone.middlewares": "ghost@file",
				},
			},
			want:       []string{"gone.example.com /*"},
			wantRoutes: 1,
			why:        "two routers share one service: the middleware reference fails closed for its own router only, and the envelope is scoped to that router's host",
		}, {
			name: "unmatchable path literal widens to its host",
			cfg:  resolved.Docker{TraefikLabels: true},
			container: fakeDaemonContainer{
				name: "ph-1", ip: "10.0.0.9", port: 3000,
				labels: map[string]string{
					"traefik.enable":                      "true",
					"traefik.http.routers.ph.rule":        "Host(`ph.example.com`) && Path(`/api/{id:[0-9]+}`)",
					"traefik.http.routers.ph.middlewares": "ghost@file",
				},
			},
			want: []string{"ph.example.com /*"},
			why:  "a placeholder path is rejected, so only the host bounds the tombstone",
		}, {
			name: "wildcard host is global",
			cfg:  resolved.Docker{TraefikLabels: true},
			container: fakeDaemonContainer{
				name: "star-1", ip: "10.0.0.9", port: 3000,
				labels: map[string]string{
					"traefik.enable":                 "true",
					"traefik.http.routers.star.rule": "Host(`*`)",
				},
			},
			want: []string{" /*"},
			why:  "Host star is Traefik any-host; a literal star would miss every real hostname into Fallback",
		}, {
			name: "wildcard host keeps a sibling path",
			cfg:  resolved.Docker{TraefikLabels: true},
			container: fakeDaemonContainer{
				name: "starpath-1", ip: "10.0.0.9", port: 3000,
				labels: map[string]string{
					"traefik.enable":                     "true",
					"traefik.http.routers.starpath.rule": "Host(`*`) && PathPrefix(`/private`)",
				},
			},
			want: []string{" /private*"},
			why:  "the host is unreadable, the PathPrefix still bounds every host",
		}, {
			name: "percent path widens to its host",
			cfg:  resolved.Docker{TraefikLabels: true},
			container: fakeDaemonContainer{
				name: "pct-1", ip: "10.0.0.9", port: 3000,
				labels: map[string]string{
					"traefik.enable":                "true",
					"traefik.http.routers.pct.rule": "Host(`pct.example.com`) && Path(`/a%20b`)",
				},
			},
			want: []string{"pct.example.com /*"},
			why:  "req.URL.Path is percent-decoded, so the encoded literal would refuse nothing a client can send",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			p, srv, _ := newFakeProvider(t, &cfg, []fakeDaemonContainer{tc.container})
			mustSync(t, p)
			got := tombstoneSet(t, srv)
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("tombstones = %v, want %v\nwhy: %s", got, tc.want, tc.why)
			}
			if n := len(srv.dynamic.Load().routes); n != tc.wantRoutes {
				t.Errorf("routes = %d, want %d", n, tc.wantRoutes)
			}
		})
	}
}

// A dropped router is refused with 404; the fallback answers unclaimed traffic.
func TestDockerTombstoneRefusesBeforeFallback(t *testing.T) {
	p, srv, _ := newFakeProvider(t, &resolved.Docker{TraefikLabels: true}, []fakeDaemonContainer{{
		name: "app-1", ip: "10.0.0.9", port: 3000,
		labels: map[string]string{
			"traefik.enable":                       "true",
			"traefik.http.routers.app.rule":        "Host(`vault.example.com`)",
			"traefik.http.routers.app.middlewares": "ghost@file",
		},
	}})
	calls := fallbackServer(t, srv, nil)
	mustSync(t, p)
	router := srv.buildRouter()

	rec := runRequest(t, router, httptest.NewRequest("GET", "http://vault.example.com/secret", nil))
	if rec.Code != http.StatusNotFound || calls.Load() != 0 {
		t.Errorf("refused request: code=%d calls=%d, want 404 with the fallback untouched", rec.Code, calls.Load())
	}
	// Traefik Host() folds one trailing FQDN dot on the request.
	rec = runRequest(t, router, httptest.NewRequest("GET", "http://vault.example.com./secret", nil))
	if rec.Code != http.StatusNotFound || calls.Load() != 0 {
		t.Errorf("absolute host: code=%d calls=%d, want 404 with the fallback untouched", rec.Code, calls.Load())
	}
	rec = runRequest(t, router, httptest.NewRequest("GET", "http://other.example.com/", nil))
	if rec.Code != http.StatusTeapot || calls.Load() != 1 {
		t.Errorf("unclaimed request: code=%d calls=%d, want 418 from the fallback", rec.Code, calls.Load())
	}
}

// A rejected router's envelope is consulted after the generation's real routes.
func TestDockerTombstoneKeepsSiblingsServing(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("served"))
	}))
	t.Cleanup(backend.Close)
	host, port := backendHostPort(t, backend)

	p, srv, _ := newFakeProvider(t, &resolved.Docker{TraefikLabels: true}, []fakeDaemonContainer{{
		name: "app-1", ip: host, port: port,
		labels: map[string]string{
			"traefik.enable":                 "true",
			"traefik.http.routers.good.rule": "Host(`good.example.com`)",
			"traefik.http.routers.bad.rule":  "ClientIP(`10.0.0.0/8`)",
		},
	}})
	calls := fallbackServer(t, srv, Routes{Match("/healthz").Handle(noContentHandler)})
	mustSync(t, p)
	router := srv.buildRouter()

	if got := tombstoneSet(t, srv); len(got) != 1 || got[0] != " /*" {
		t.Fatalf("tombstones = %v, want one global refusal", got)
	}
	rec := runRequest(t, router, httptest.NewRequest("GET", "http://good.example.com/", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "served" {
		t.Errorf("sibling router: code=%d body=%q, want the backend's response", rec.Code, rec.Body.String())
	}
	// Static routes are compiled ahead of the whole dynamic table.
	if rec := runRequest(t, router, httptest.NewRequest("GET", "http://any.example.com/healthz", nil)); rec.Code != http.StatusNoContent {
		t.Errorf("static route: code=%d, want 204", rec.Code)
	}
	if rec := runRequest(t, router, httptest.NewRequest("GET", "http://any.example.com/", nil)); rec.Code != http.StatusNotFound {
		t.Errorf("unmatched request: code=%d, want the global refusal's 404", rec.Code)
	}
	if calls.Load() != 0 {
		t.Errorf("fallback ran %d times, want 0 while a global tombstone stands", calls.Load())
	}
}

// A tombstone belongs to the generation that derived it and is swapped with it.
func TestDockerTombstoneGenerationReplacement(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("served"))
	}))
	t.Cleanup(backend.Close)
	host, port := backendHostPort(t, backend)

	broken := fakeDaemonContainer{
		name: "app-1", ip: host, port: port,
		labels: map[string]string{
			"traefik.enable":                "true",
			"traefik.http.routers.app.rule": "Host(`app.example.com`) && ClientIP(`10.0.0.0/8`)",
		},
	}
	fixed := fakeDaemonContainer{
		name: "app-1", ip: host, port: port,
		labels: map[string]string{
			"traefik.enable":                "true",
			"traefik.http.routers.app.rule": "Host(`app.example.com`)",
		},
	}
	p, srv, setContainers := newFakeProvider(t, &resolved.Docker{TraefikLabels: true}, []fakeDaemonContainer{broken})
	calls := fallbackServer(t, srv, nil)
	mustSync(t, p)
	router := srv.buildRouter()

	if rec := runRequest(t, router, httptest.NewRequest("GET", "http://app.example.com/", nil)); rec.Code != http.StatusNotFound {
		t.Fatalf("first generation: code=%d, want the tombstone's 404", rec.Code)
	}

	setContainers([]fakeDaemonContainer{fixed})
	mustSync(t, p)
	if got := tombstoneSet(t, srv); len(got) != 0 {
		t.Errorf("second generation kept tombstones %v", got)
	}
	rec := runRequest(t, router, httptest.NewRequest("GET", "http://app.example.com/", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "served" {
		t.Errorf("second generation: code=%d body=%q, want the backend's response", rec.Code, rec.Body.String())
	}
	if rec := runRequest(t, router, httptest.NewRequest("GET", "http://other.example.com/", nil)); rec.Code != http.StatusTeapot {
		t.Errorf("second generation fallback: code=%d, want 418", rec.Code)
	}
	if calls.Load() != 1 {
		t.Errorf("fallback ran %d times, want exactly the one request after the swap", calls.Load())
	}
}

// appRouter is one container exposing a Traefik router with the given rule:
// the fixture the repair tests move between a rejected and an accepted rule.
func appRouter(rule string) fakeDaemonContainer {
	return fakeDaemonContainer{
		name: "app-1", ip: "10.0.0.9", port: 3000,
		labels: map[string]string{
			"traefik.enable":                "true",
			"traefik.http.routers.app.rule": rule,
		},
	}
}

// The standing refusal is a fact about the current generation. A rule that
// is repaired and later regresses is announced again.
func TestDockerTombstoneReannouncesAfterRepair(t *testing.T) {
	var logs bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(oldOutput) })

	broken := appRouter("Host(`app.example.com`) && ClientIP(`10.0.0.0/8`)")
	fixed := appRouter("Host(`app.example.com`)")
	const announcement = "generation: routes dropped, refusing app.example.com/*"
	announcements := func() int { return strings.Count(logs.String(), announcement) }

	p, _, setContainers := newFakeProvider(t, &resolved.Docker{TraefikLabels: true}, []fakeDaemonContainer{broken})
	mustSync(t, p)
	if got := announcements(); got != 1 {
		t.Fatalf("first generation announced the refusal %d times, want 1", got)
	}
	mustSync(t, p)
	if got := announcements(); got != 1 {
		t.Errorf("an unchanged refusal announced %d times, want 1 — a generation is rebuilt every poll", got)
	}
	setContainers([]fakeDaemonContainer{fixed})
	mustSync(t, p)
	setContainers([]fakeDaemonContainer{broken})
	mustSync(t, p)
	if got := announcements(); got != 2 {
		t.Errorf("a refusal that returned after a repair announced %d times in total, want 2", got)
	}
}

// The clearing edge is announced on the transition and only there.
func TestDockerTombstoneAnnouncesRepair(t *testing.T) {
	var logs bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(oldOutput) })

	broken := appRouter("Host(`app.example.com`) && ClientIP(`10.0.0.0/8`)")
	fixed := appRouter("Host(`app.example.com`)")
	const cleared = "refusals cleared"
	repairs := func() int { return strings.Count(logs.String(), cleared) }

	p, srv, setContainers := newFakeProvider(t, &resolved.Docker{TraefikLabels: true}, []fakeDaemonContainer{fixed})
	mustSync(t, p)
	if got := repairs(); got != 0 {
		t.Fatalf("a provider that never refused anything announced a repair %d times, want 0", got)
	}
	mustSync(t, p)
	if got := repairs(); got != 0 {
		t.Fatalf("polling without a refusal announced a repair %d times, want 0", got)
	}

	setContainers([]fakeDaemonContainer{broken})
	mustSync(t, p)
	if got := tombstoneSet(t, srv); len(got) != 1 {
		t.Fatalf("tombstones = %v, want the rejected router refused", got)
	}
	if got := repairs(); got != 0 {
		t.Fatalf("the onset of a refusal announced a repair %d times, want 0", got)
	}

	setContainers([]fakeDaemonContainer{fixed})
	mustSync(t, p)
	if got := tombstoneSet(t, srv); len(got) != 0 {
		t.Fatalf("tombstones = %v after the repair, want none", got)
	}
	if got := repairs(); got != 1 {
		t.Fatalf("the repair was announced %d times, want 1", got)
	}
	mustSync(t, p)
	if got := repairs(); got != 1 {
		t.Errorf("an unchanged all-clear generation announced the repair %d times, want 1 — a generation is rebuilt every poll", got)
	}
}

// Two containers rejected the same way leave one refusal. A global envelope
// absorbs the rest.
func TestDockerTombstoneDedupesAcrossContainers(t *testing.T) {
	p, srv, _ := newFakeProvider(t, &resolved.Docker{TraefikLabels: true}, []fakeDaemonContainer{
		{
			name: "a-1", ip: "10.0.0.9", port: 3000,
			labels: map[string]string{
				"traefik.enable":                "true",
				"traefik.http.routers.one.rule": "Host(`dup.example.com`) && ClientIP(`10.0.0.0/8`)",
			},
		},
		{
			name: "a-2", ip: "10.0.0.10", port: 3000,
			labels: map[string]string{
				"traefik.enable":                "true",
				"traefik.http.routers.two.rule": "Host(`dup.example.com`) && Header(`X`, `y`)",
			},
		},
	})
	mustSync(t, p)
	if got := tombstoneSet(t, srv); len(got) != 1 || got[0] != "dup.example.com /*" {
		t.Fatalf("tombstones = %v, want a single deduplicated refusal", got)
	}

	p2, srv2, _ := newFakeProvider(t, &resolved.Docker{TraefikLabels: true}, []fakeDaemonContainer{
		{
			name: "b-1", ip: "10.0.0.9", port: 3000,
			labels: map[string]string{
				"traefik.enable":                 "true",
				"traefik.http.routers.host.rule": "Host(`dup.example.com`) && ClientIP(`10.0.0.0/8`)",
				"traefik.http.routers.wide.rule": "ClientIP(`10.0.0.0/8`)",
			},
		},
	})
	mustSync(t, p2)
	if got := tombstoneSet(t, srv2); len(got) != 1 || got[0] != " /*" {
		t.Fatalf("tombstones = %v, want the global refusal to absorb the host-scoped one", got)
	}
}

// A rejected PathPrefix(`/admin`) refuses Traefik's byte prefix, including
// /admin-secret. Statute Match("/admin/*") would miss that path.
func TestDockerTombstonePathPrefixIsBytePrefix(t *testing.T) {
	p, srv, _ := newFakeProvider(t, &resolved.Docker{TraefikLabels: true}, []fakeDaemonContainer{{
		name: "app-1", ip: "10.0.0.9", port: 3000,
		labels: map[string]string{
			"traefik.enable":                       "true",
			"traefik.http.routers.app.rule":        "Host(`admin.example.com`) && PathPrefix(`/admin`)",
			"traefik.http.routers.app.middlewares": "ghost@file",
		},
	}})
	calls := fallbackServer(t, srv, nil)
	mustSync(t, p)
	if got := tombstoneSet(t, srv); len(got) != 1 || got[0] != "admin.example.com /admin*" {
		t.Fatalf("tombstones = %v, want admin.example.com /admin*", got)
	}
	router := srv.buildRouter()
	for _, path := range []string{"/admin", "/admin/", "/admin/users", "/admin-secret"} {
		rec := runRequest(t, router, httptest.NewRequest("GET", "http://admin.example.com"+path, nil))
		if rec.Code != http.StatusNotFound || calls.Load() != 0 {
			t.Errorf("%s: code=%d calls=%d, want 404 with the fallback untouched", path, rec.Code, calls.Load())
		}
	}
	rec := runRequest(t, router, httptest.NewRequest("GET", "http://other.example.com/admin-secret", nil))
	if rec.Code != http.StatusTeapot || calls.Load() != 1 {
		t.Errorf("unclaimed: code=%d calls=%d, want 418 from the fallback", rec.Code, calls.Load())
	}
}

// A valid PathPrefix(`/admin`) serves /admin-secret. A trailing FQDN dot
// on the request host does not miss the route into the fallback.
func TestDockerTraefikPathPrefixAndHostDotServe(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("served"))
	}))
	t.Cleanup(backend.Close)
	host, port := backendHostPort(t, backend)

	p, srv, _ := newFakeProvider(t, &resolved.Docker{TraefikLabels: true}, []fakeDaemonContainer{{
		name: "app-1", ip: host, port: port,
		labels: map[string]string{
			"traefik.enable":                "true",
			"traefik.http.routers.app.rule": "Host(`admin.example.com.`) && PathPrefix(`/admin`)",
		},
	}})
	calls := fallbackServer(t, srv, nil)
	mustSync(t, p)
	router := srv.buildRouter()

	for _, reqHost := range []string{"admin.example.com", "admin.example.com.", "admin.example.com.."} {
		rec := runRequest(t, router, httptest.NewRequest("GET", "http://"+reqHost+"/admin-secret", nil))
		if rec.Code != http.StatusOK || rec.Body.String() != "served" {
			t.Errorf("host %q /admin-secret: code=%d body=%q, want the backend", reqHost, rec.Code, rec.Body.String())
		}
	}
	rec := runRequest(t, router, httptest.NewRequest("GET", "http://admin.example.com/other", nil))
	if rec.Code != http.StatusTeapot || calls.Load() != 1 {
		t.Errorf("outside prefix: code=%d calls=%d, want 418 from the fallback", rec.Code, calls.Load())
	}
}

// A native /api/* tombstone and a Traefik PathPrefix(`/api`) tombstone in
// one generation keep the byte prefix: /api-secret must not reach Fallback.
func TestDockerTombstoneMixedNativeAndTraefikAbsorbsBytePrefix(t *testing.T) {
	p, srv, _ := newFakeProvider(t, &resolved.Docker{TraefikLabels: true}, []fakeDaemonContainer{
		{
			name: "native-1", ip: "10.0.0.8",
			labels: map[string]string{
				"statute.enable": "true",
				"statute.host":   "mix.example.com",
				"statute.path":   "/api/*",
			},
		},
		{
			name: "tfk-1", ip: "10.0.0.9", port: 3000,
			labels: map[string]string{
				"traefik.enable":                       "true",
				"traefik.http.routers.api.rule":        "Host(`mix.example.com`) && PathPrefix(`/api`)",
				"traefik.http.routers.api.middlewares": "ghost@file",
			},
		},
	})
	calls := fallbackServer(t, srv, nil)
	mustSync(t, p)
	if got := tombstoneSet(t, srv); len(got) != 1 || got[0] != "mix.example.com /api*" {
		t.Fatalf("tombstones = %v, want mix.example.com /api*", got)
	}
	router := srv.buildRouter()
	for _, path := range []string{"/api", "/api/users", "/api-secret"} {
		rec := runRequest(t, router, httptest.NewRequest("GET", "http://mix.example.com"+path, nil))
		if rec.Code != http.StatusNotFound || calls.Load() != 0 {
			t.Errorf("%s: code=%d calls=%d, want 404 with the fallback untouched", path, rec.Code, calls.Load())
		}
	}
}
