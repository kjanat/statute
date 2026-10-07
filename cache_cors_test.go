package statute

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"statute.kjanat.dev/internal/docker"
)

func TestCacheCORSVariance(t *testing.T) {
	t.Parallel()
	cors := CORS().Origins("https://a.example", "https://b.example")
	for index, order := range [][]Middleware{
		{cors, Cache("1h")},
		{Cache("1h"), cors},
		{cors, Retry(2), Cache("1h")},
		{cors, ETag(), Cache("1h")},
		{Cache("1h"), Retry(2), cors},
		{Cache("1h"), ETag(), cors},
		{Cache("1h"), cors, ETag(), Cache("1h")},
	} {
		for _, op := range []Middleware{AddResponseHeader("Vary", "X-Language"), SetResponseHeader("Vary", "X-Language"), RemoveResponseHeader("Vary")} {
			t.Run(fmt.Sprintf("order=%d/%T", index, op), func(t *testing.T) {
				t.Parallel()
				calls := 0
				h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					w.Header().Set("Vary", "X-Language")
					switch r.Header.Get("Origin") {
					case "https://a.example":
						w.Header().Set("X-Selected", "a")
					case "https://b.example":
						w.Header().Set("X-Selected", "b")
					default:
						w.Header().Set("X-Selected", "none")
					}
				}), append(slices.Clone(order), op)...)
				for _, tc := range []struct{ origin, selected string }{
					{"", "none"}, {"https://a.example", "a"}, {"https://b.example", "b"},
					{"", "none"}, {"https://a.example", "a"}, {"https://b.example", "b"},
				} {
					r := httptest.NewRequest("GET", "/", nil)
					if tc.origin != "" {
						r.Header.Set("Origin", tc.origin)
					}
					rec := runRequest(t, h, r)
					assertCacheCORSSelection(t, rec, tc.origin, tc.selected)
				}
				if calls != 3 {
					t.Fatalf("variant reuse lost: %d calls", calls)
				}
			})
		}
	}
}

func assertCacheCORSSelection(t *testing.T, rec *httptest.ResponseRecorder, origin, selected string) {
	t.Helper()
	if rec.Code != 200 || rec.Header().Get("X-Selected") != selected || rec.Header().Get("Access-Control-Allow-Origin") != origin {
		t.Fatalf("origin=%q selected=%q status=%d headers=%v", origin, selected, rec.Code, rec.Header())
	}
	names, valid := cacheVary(rec.Header())
	if !valid || !slices.Contains(names, "origin") {
		t.Fatalf("missing final Origin variance: %v", rec.Header())
	}
}

func TestCacheCORSOuterOriginBody(t *testing.T) {
	t.Parallel()
	calls := 0
	h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Origin") == "https://a.example" {
			_, _ = io.WriteString(w, "account A")
			return
		}
		_, _ = io.WriteString(w, "account B")
	}), CORS().Origins("https://a.example", "https://b.example"), ETag(), Retry(2), Cache("1h"))
	for _, tc := range []struct{ origin, body string }{
		{"https://a.example", "account A"}, {"https://b.example", "account B"}, {"https://a.example", "account A"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Origin", tc.origin)
		rec := runRequest(t, h, r)
		if rec.Body.String() != tc.body || rec.Header().Get("Access-Control-Allow-Origin") != tc.origin {
			t.Fatalf("origin=%s body=%q headers=%v", tc.origin, rec.Body.String(), rec.Header())
		}
	}
	if calls != 2 {
		t.Fatalf("calls=%d want=2", calls)
	}
}

func TestCORSMandatoryVaryEmptyAndRepeatedFields(t *testing.T) {
	t.Parallel()
	h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header()["Vary"] = []string{"Accept-Encoding", "X-Language"}
		w.Header()["vArY"] = []string{"X-Tenant"}
	}), CORS().Origins("https://a.example"))
	rec := runRequest(t, h, httptest.NewRequest("GET", "/", nil))
	names, valid := cacheVary(rec.Header())
	if !valid || !slices.Equal(names, []string{"accept-encoding", "origin", "x-language", "x-tenant"}) {
		t.Fatalf("vary=%v valid=%v", names, valid)
	}
}

func TestDockerCacheCORSRouteIsolation(t *testing.T) {
	t.Parallel()
	cfg, err := resolveDocker(Docker().DefaultMiddleware(Cache("1h")).Middleware("cors", CORS().Origins("https://a.example", "https://b.example")))
	if err != nil {
		t.Fatal(err)
	}
	p := &dockerProvider{cfg: cfg}
	for _, tc := range []struct {
		names []string
		calls int
	}{
		{nil, 1}, {[]string{"cors"}, 2},
	} {
		enabled := len(tc.names) > 0
		mws, warning := p.routeMiddleware(&docker.Service{Name: "shared"}, docker.Matcher{Middlewares: tc.names}, nil)
		if warning != "" {
			t.Fatal(warning)
		}
		calls := 0
		h := wrapMiddleware(mws, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			if enabled {
				if r.Header.Get("Origin") == "https://a.example" {
					_, _ = io.WriteString(w, "https://a.example")
					return
				}
				_, _ = io.WriteString(w, "https://b.example")
			}
		}))
		for _, origin := range []string{"https://a.example", "https://b.example", "https://a.example"} {
			r := httptest.NewRequest("GET", "/", nil)
			r.Header.Set("Origin", origin)
			rec := runRequest(t, h, r)
			assertDockerCacheCORS(t, rec, origin, enabled)
		}
		if calls != tc.calls {
			t.Fatalf("cors=%v calls=%d want=%d", enabled, calls, tc.calls)
		}
	}
}

func assertDockerCacheCORS(t *testing.T, rec *httptest.ResponseRecorder, origin string, enabled bool) {
	t.Helper()
	if enabled && rec.Body.String() != origin {
		t.Fatalf("wrong Docker variant: %q", rec.Body.String())
	}
	if !enabled && rec.Header().Get("Vary") != "" {
		t.Fatal("CORS variance leaked to sibling route")
	}
}

func TestResponseHeaderOperationsOnEmptyReturn(t *testing.T) {
	t.Parallel()
	h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Policy", "origin")
	}), SetResponseHeader("X-Policy", "route"))
	rec := runRequest(t, h, httptest.NewRequest("GET", "/", nil))
	if got := rec.Result().Header.Get("X-Policy"); got != "route" {
		t.Fatalf("policy=%q want route", got)
	}
}
