package traefikoracle

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"statute.kjanat.dev/internal/docker"
)

type probe struct {
	host    string
	path    string
	headers map[string]string
}

var differentialProbes = []probe{
	{host: "app.example.com", path: "/"},
	{host: "APP.EXAMPLE.COM", path: "/api"},
	{host: "app.example.com.", path: "/api/v1"},
	{host: "app.example.com..", path: "/api-secret"},
	{host: "app.example.com...", path: "/login"},
	{host: "a.example.com", path: "/"},
	{host: "a.example.com.", path: "/api"},
	{host: "b.example.com", path: "/login"},
	{host: "c.example.com", path: "/api"},
	{host: "0", path: "/"},
	{host: "0.", path: "/"},
	{host: "0..", path: "/"},
	{host: "0...", path: "/"},
	{host: "other.example.com", path: "/"},
	{host: "other.example.com", path: "/login"},
	{host: "other.example.com", path: "/login/"},
	{host: "other.example.com", path: "/api"},
	{host: "other.example.com", path: "/api/"},
	{host: "other.example.com", path: "/api/v1"},
	{host: "other.example.com", path: "/api-secret"},
	{host: "other.example.com", path: "/ap"},
	{host: "other.example.com", path: "/x"},
	{host: "other.example.com", path: "/x/"},
	{host: "other.example.com", path: "/x-secret"},
}

func TestAcceptedRulesMatchTraefik(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		rule   string
		syntax string
	}{
		{name: "host", rule: "Host(`app.example.com`)"},
		{name: "dotted host", rule: "Host(`app.example.com.`)"},
		{name: "exact path", rule: "Path(`/login`)"},
		{name: "byte path prefix", rule: "PathPrefix(`/api`)"},
		{name: "host and path prefix", rule: "Host(`app.example.com`) && PathPrefix(`/api`)"},
		{name: "parenthesized disjunction", rule: "(Host(`a.example.com`) || Host(`b.example.com`)) && (Path(`/login`) || PathPrefix(`/api`))"},
		{name: "operator precedence", rule: "Host(`a.example.com`) && Path(`/x`) || Host(`b.example.com`)"},
		{name: "double-dot regression", rule: "Host(`0..`) || Host(`0`)"},
		{name: "v2 multi-argument host", rule: "Host(`a.example.com`, `b.example.com`)", syntax: "v2"},
		{name: "v2 multi-argument host and prefix", rule: "Host(`a.example.com`, `b.example.com`) && PathPrefix(`/api`)", syntax: "v2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			statuteMatchers, err := docker.ParseRule(tc.rule)
			if err != nil {
				t.Fatalf("Statute rejected accepted rule %q: %v", tc.rule, err)
			}
			traefik, err := compileTraefik(tc.rule, tc.syntax)
			if err != nil {
				t.Fatalf("Traefik rejected accepted rule %q: %v", tc.rule, err)
			}
			traefikHits := 0
			for _, p := range differentialProbes {
				got := matchesAny(statuteMatchers, p)
				want := traefikMatches(traefik, p)
				if want {
					traefikHits++
				}
				if got != want {
					t.Errorf("rule %q, request host=%q path=%q: Statute=%v Traefik=%v", tc.rule, p.host, p.path, got, want)
				}
			}
			if traefikHits == 0 {
				t.Fatalf("Traefik matched none of the differential probes for %q", tc.rule)
			}
		})
	}
}

func TestRejectedRulesAreProbedAgainstTraefik(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		rule           string
		traefikAccepts bool
		probes         []probe
	}{
		{
			name:           "wildcard host",
			rule:           "Host(`*`)",
			traefikAccepts: true,
			probes:         []probe{{host: "anything.example.com", path: "/private"}},
		}, {
			name:           "wildcard suffix",
			rule:           "Host(`*.example.com`)",
			traefikAccepts: true,
			probes:         []probe{{host: "api.example.com", path: "/"}},
		}, {
			name:           "percent path",
			rule:           "Host(`pct.example.com`) && Path(`/a%2Fb`)",
			traefikAccepts: true,
			probes:         []probe{{host: "pct.example.com", path: "/a%2Fb"}},
		}, {
			name:           "header matcher",
			rule:           "Host(`admin.example.com`) && Header(`X-Probe`, `ok`)",
			traefikAccepts: true,
			probes:         []probe{{host: "admin.example.com", path: "/private", headers: map[string]string{"X-Probe": "ok"}}},
		}, {
			name:           "host regexp with path prefix",
			rule:           "HostRegexp(`^admin[.]example[.]com$`) && PathPrefix(`/private`)",
			traefikAccepts: true,
			probes:         []probe{{host: "admin.example.com", path: "/private-secret"}},
		}, {
			name:           "negation",
			rule:           "!Host(`private.example.com`)",
			traefikAccepts: true,
			probes:         []probe{{host: "public.example.com", path: "/"}},
		}, {
			name:           "path placeholder",
			rule:           "Host(`api.example.com`) && Path(`/users/{id}`)",
			traefikAccepts: true,
			probes:         []probe{{host: "api.example.com", path: "/users/{id}"}},
		}, {
			name: "missing comma",
			rule: "Host(`a.example.com``b.example.com`)",
		}, {
			name: "non-ASCII host",
			rule: "Host(`café.example.com`)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := docker.ParseRule(tc.rule); err == nil {
				t.Fatalf("Statute unexpectedly accepted rejected rule %q", tc.rule)
			}
			traefik, err := compileTraefik(tc.rule, "")
			if !tc.traefikAccepts {
				if err == nil {
					t.Fatalf("Traefik unexpectedly accepted invalid rule %q", tc.rule)
				}
				return
			}
			if err != nil {
				t.Fatalf("Traefik rejected probe rule %q: %v", tc.rule, err)
			}

			envelope := docker.RuleEnvelope(tc.rule)
			matched := 0
			for _, p := range tc.probes {
				if !traefikMatches(traefik, p) {
					continue
				}
				matched++
				if !matchesAny(envelope, p) {
					t.Errorf("rule %q: Traefik matched host=%q path=%q outside Statute envelope %+v", tc.rule, p.host, p.path, envelope)
				}
			}
			if matched == 0 {
				t.Fatalf("Traefik matched none of the probes for %q", tc.rule)
			}
		})
	}
}

func traefikMatches(handler http.Handler, p probe) bool {
	req := httptest.NewRequest(http.MethodGet, "http://oracle.invalid"+p.path, nil)
	req.Host = p.host
	for name, value := range p.headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Code == http.StatusNoContent
}

func matchesAny(matchers []docker.Matcher, p probe) bool {
	for _, matcher := range matchers {
		if matcher.Match(p.host, p.path) {
			return true
		}
	}
	return false
}
