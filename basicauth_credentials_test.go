package statute

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"statute.kjanat.dev/internal/docker"
)

func TestBasicAuthHoistedCredentialBypass(t *testing.T) {
	mws, err := resolveMiddlewares([]Middleware{BasicAuth("private", map[string]string{"alice": hunter2Hash}), SetRequestHeader("Authorization", "Basic YWxpY2U6aHVudGVyMg==")})
	if err != nil {
		assertCredentialConflict(t, err, "direct rewrite")
		return
	}
	h := wrapMiddleware(mws, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	rec := runRequest(t, h, httptest.NewRequest("GET", "/", nil))
	t.Fatalf("unsafe chain accepted: unauthenticated request received %d", rec.Code)
}

func TestResolveBasicAuthHoistedCredentials(t *testing.T) {
	auth := BasicAuth("private", map[string]string{"alice": hunter2Hash})
	for _, name := range []string{"Authorization", "aUtHoRiZaTiOn"} {
		for _, op := range []Middleware{SetRequestHeader(name, "Basic YWxpY2U6aHVudGVyMg=="), AddRequestHeader(name, "Basic YWxpY2U6aHVudGVyMg=="), RemoveRequestHeader(name)} {
			for _, before := range []bool{false, true} {
				for _, fallback := range []bool{false, true} {
					mws := []Middleware{auth, op}
					if before {
						mws[0], mws[1] = mws[1], mws[0]
					}
					cfg := Config{Listeners: Listeners{HTTP(":0")}, Routes: Routes{Match("/*").Handle(http.NotFoundHandler()).With(mws...)}}
					if fallback {
						cfg.FallbackRoutes, cfg.Routes = cfg.Routes, nil
					}
					_, err := Resolve(cfg)
					assertCredentialConflict(t, err, fmt.Sprintf("%s %T before=%t fallback=%t", name, op, before, fallback))
				}
			}
		}
	}
}

func assertCredentialConflict(t *testing.T, err error, context string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "BasicAuth credentials") {
		t.Fatalf("%s: expected credential conflict, got %v", context, err)
	}
	if strings.Contains(err.Error(), "YWxpY2U6") {
		t.Fatal("credential value leaked into configuration error")
	}
}

func TestBasicAuthHoistedCredentialMapping(t *testing.T) {
	auth := BasicAuth("private", map[string]string{"alice": hunter2Hash})
	for _, op := range []Middleware{SetRequestHeader("x-Credential", "Basic YWxpY2U6aHVudGVyMg=="), AddRequestHeader("X-CREDENTIAL", "Basic YWxpY2U6aHVudGVyMg=="), RemoveRequestHeader("X-Credential")} {
		_, err := resolveMiddlewares([]Middleware{RequestID().Header("authorization").From("X-Credential"), Retry(2), ETag(), auth, op})
		assertCredentialConflict(t, err, "indirect credential mapping")
	}
}

func TestBasicAuthCredentialRewriteControls(t *testing.T) {
	auth := BasicAuth("private", map[string]string{"alice": hunter2Hash})
	for _, mws := range [][]Middleware{
		{SetRequestHeader("Authorization", "upstream-only")},
		{auth, SetResponseHeader("Authorization", "response-only")},
		{auth, SetRequestHeader("X-Trace", "trace")},
		{RequestID().Header("Authorization").From("X-Credential"), auth},
		{auth, RequestID().Header("Authorization").From("X-Credential"), SetRequestHeader("X-Credential", "upstream-only")},
	} {
		if _, err := resolveMiddlewares(mws); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBasicAuthBeforeUpstreamCredentialMapping(t *testing.T) {
	mws, err := resolveMiddlewares([]Middleware{BasicAuth("private", map[string]string{"alice": hunter2Hash}), Retry(2), RequestID().Header("Authorization").From("X-Upstream"), SetRequestHeader("X-Upstream", "upstream-token")})
	if err != nil {
		t.Fatal(err)
	}
	h := wrapMiddleware(mws, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "upstream-token" {
			t.Error("upstream credential mapping lost")
		}
		w.WriteHeader(http.StatusOK)
	}))
	for _, valid := range []bool{true, false} {
		req := httptest.NewRequest("GET", "/", nil)
		want := http.StatusUnauthorized
		if valid {
			req.SetBasicAuth("alice", "hunter2")
			want = http.StatusOK
		}
		original := req.Header.Get("Authorization")
		if rec := runRequest(t, h, req); rec.Code != want {
			t.Fatalf("valid=%t status=%d want=%d", valid, rec.Code, want)
		}
		if req.Header.Get("Authorization") != original {
			t.Fatal("upstream mapping overwrote the caller's credentials")
		}
	}
}

func TestDockerBasicAuthMappedCredentialAssembly(t *testing.T) {
	cfg, err := resolveDocker(Docker().
		Middleware("auth", BasicAuth("private", map[string]string{"alice": hunter2Hash})).
		Middleware("mapping", RequestID().Header("Authorization").From("X-Credential")).
		Middleware("writer", SetRequestHeader("X-Credential", "Basic YWxpY2U6aHVudGVyMg==")))
	if err != nil {
		t.Fatal(err)
	}
	p := &dockerProvider{cfg: cfg}
	for _, names := range [][]string{{"mapping", "auth", "writer"}, {"writer", "mapping", "auth"}, {"auth", "mapping", "writer", "auth"}} {
		_, warning := p.routeMiddleware(&docker.Service{Name: "app"}, docker.Matcher{Middlewares: names}, nil)
		if !strings.Contains(warning, "BasicAuth credentials") {
			t.Fatalf("%v accepted: %q", names, warning)
		}
	}
	if _, warning := p.routeMiddleware(&docker.Service{Name: "app"}, docker.Matcher{Middlewares: []string{"auth", "mapping", "writer"}}, nil); warning != "" {
		t.Fatal(warning)
	}
}

func TestBasicAuthMappedCredentialsWithCacheRetry(t *testing.T) {
	mws, err := resolveMiddlewares([]Middleware{RequestID().Header("Authorization").From("X-Credential"), Cache("1h"), Retry(2, OnStatus(503)), BasicAuth("private", map[string]string{"alice": hunter2Hash}), AddRequestHeader("X-Once", "one")})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	h := wrapMiddleware(mws, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if got := r.Header.Values("X-Once"); len(got) != 1 || got[0] != "one" {
			t.Errorf("header operation repeated: %v", got)
		}
		if calls%2 != 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	for _, credential := range []string{"Basic YWxpY2U6aHVudGVyMg==", "", "Basic YWxpY2U6d3Jvbmc=", "Basic YWxpY2U6aHVudGVyMg=="} {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("X-Credential", credential)
		want := http.StatusUnauthorized
		if credential == "Basic YWxpY2U6aHVudGVyMg==" { //nolint:gosec // G101: deliberate test credential, not a production secret
			want = http.StatusOK
		}
		if rec := runRequest(t, h, req); rec.Code != want {
			t.Fatalf("status=%d want=%d", rec.Code, want)
		}
	}
	if calls != 4 {
		t.Fatalf("credential responses cached or Retry lost: calls=%d", calls)
	}
}

func TestDockerBasicAuthCredentialRefusal(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(backend.Close)
	host, portText, err := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	for _, defaults := range []bool{false, true} {
		t.Run(fmt.Sprint(defaults), func(t *testing.T) {
			d := Docker().TraefikLabels().Middleware("auth", BasicAuth("private", map[string]string{"alice": hunter2Hash}))
			writer := SetRequestHeader("Authorization", "Basic YWxpY2U6aHVudGVyMg==")
			chain := "auth,writer"
			if defaults {
				d.DefaultMiddleware(writer)
				chain = "auth"
			} else {
				d.Middleware("writer", writer)
			}
			cfg, err := resolveDocker(d)
			if err != nil {
				t.Fatal(err)
			}
			containers := []fakeDaemonContainer{{name: "app", ip: host, port: port, labels: map[string]string{
				"traefik.enable":                         "true",
				"traefik.http.routers.admin.rule":        "Host(`app.example.com`) && PathPrefix(`/admin`)",
				"traefik.http.routers.admin.service":     "app",
				"traefik.http.routers.admin.middlewares": chain,
				"traefik.http.routers.public.rule":       "Host(`app.example.com`) && PathPrefix(`/`)",
				"traefik.http.routers.public.service":    "app",
			}}}
			p, srv, update := newFakeProvider(t, cfg, containers)
			mustSync(t, p)
			assertCredentialRouteStatus(t, srv, "/admin", http.StatusNotFound)
			assertCredentialRouteStatus(t, srv, "/public", http.StatusOK)
			if defaults {
				p.cfg.DefaultMiddleware = nil
			}
			containers[0].labels["traefik.http.routers.admin.middlewares"] = "auth"
			update(containers)
			mustSync(t, p)
			assertCredentialRouteStatus(t, srv, "/admin", http.StatusUnauthorized)
			assertCredentialRouteStatus(t, srv, "/public", http.StatusOK)
		})
	}
}

func assertCredentialRouteStatus(t *testing.T, srv *server, path string, want int) {
	t.Helper()
	req := httptest.NewRequest("GET", "http://app.example.com"+path, nil)
	h := findDynamicHandler(srv.dynamic.Load(), req.Host, req)
	if h == nil {
		t.Fatalf("%s fell through to fallback", path)
	}
	if rec := runRequest(t, h, req); rec.Code != want {
		t.Fatalf("%s status=%d want=%d", path, rec.Code, want)
	}
}
