package statute

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"statute.kjanat.dev/internal/docker"
)

func priorityFixtureRoute(service, pattern string, priority, status int) compiledRoute {
	m := docker.CompileNative("app.example", pattern)
	m.Priority, m.PriorityOrder = priority, true
	return compiledRoute{service: service, matcher: m, handler: statusHandler(status)}
}

func TestPriorityQuarantineKeepsHealthySameService(t *testing.T) {
	healthy := priorityFixtureRoute("shared", "/*", 10, http.StatusOK)
	quarantine := priorityFixtureRoute("shared", "/*", 10, http.StatusServiceUnavailable)
	other := priorityFixtureRoute("other", "/*", 10, http.StatusServiceUnavailable)
	for _, tc := range []struct {
		name                string
		routes, quarantines []compiledRoute
		status              int
	}{
		{"same service", []compiledRoute{healthy}, []compiledRoute{quarantine}, 200},
		{"different service", []compiledRoute{healthy}, []compiledRoute{other}, 503},
		{"every quarantine", []compiledRoute{healthy}, []compiledRoute{quarantine, other}, 503},
		{"each service has a healthy contributor", []compiledRoute{healthy, priorityFixtureRoute("other", "/*", 10, 200)}, []compiledRoute{quarantine, other}, 200},
		{"unrelated tie cannot borrow a healthy service", []compiledRoute{priorityFixtureRoute("unrelated", "/*", 10, 201), healthy}, []compiledRoute{quarantine}, 200},
		{"reverse quarantine order", []compiledRoute{healthy}, []compiledRoute{other, quarantine}, 503},
		{"higher serving priority", []compiledRoute{priorityFixtureRoute("shared", "/*", 20, 200)}, []compiledRoute{other}, 200},
		{"healthy sibling has lower priority", []compiledRoute{priorityFixtureRoute("shared", "/*", 5, 200)}, []compiledRoute{quarantine}, 200},
		{"specific quarantine", []compiledRoute{healthy}, []compiledRoute{priorityFixtureRoute("shared", "/admin/*", 10, 503)}, 503},
		{"nonmatching quarantine", []compiledRoute{healthy}, []compiledRoute{priorityFixtureRoute("other", "/other/*", 20, 503)}, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://app.example/admin/item", nil)
			chosen := priorityDynamicRoute(&dynamicTable{routes: tc.routes, quarantines: tc.quarantines}, "app.example", r)
			if chosen == nil {
				t.Fatal("matched route disappeared")
			}
			w := httptest.NewRecorder()
			chosen.handler.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status=%d want=%d", w.Code, tc.status)
			}
		})
	}
}
