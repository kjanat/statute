package docker

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestParseRule(t *testing.T) {
	tests := []struct {
		name string
		rule string
		want []Matcher
	}{
		{
			name: "host only",
			rule: "Host(`app.example.com`)",
			want: []Matcher{m("app.example.com", "/*")},
		},
		{
			name: "host and pathprefix",
			rule: "Host(`app.example.com`) && PathPrefix(`/api`)",
			want: []Matcher{px("app.example.com", "/api")},
		},
		{
			name: "exact path",
			rule: "Path(`/login`)",
			want: []Matcher{m("", "/login")},
		},
		{
			name: "multi-arg host is a disjunction",
			rule: "Host(`a.example.com`, `b.example.com`)",
			want: []Matcher{
				m("a.example.com", "/*"),
				m("b.example.com", "/*"),
			},
		},
		{
			name: "or of hosts",
			rule: "Host(`a.example.com`) || Host(`b.example.com`)",
			want: []Matcher{
				m("a.example.com", "/*"),
				m("b.example.com", "/*"),
			},
		},
		{
			name: "and distributes over or",
			rule: "(Host(`a.example.com`) || Host(`b.example.com`)) && PathPrefix(`/api`)",
			want: []Matcher{
				px("a.example.com", "/api"),
				px("b.example.com", "/api"),
			},
		},
		{
			name: "double quotes",
			rule: `Host("app.example.com")`,
			want: []Matcher{m("app.example.com", "/*")},
		},
		{
			name: "pathprefix with trailing slash is a byte prefix of that slash",
			rule: "PathPrefix(`/api/`)",
			want: []Matcher{px("", "/api/")},
		},
		{
			name: "root pathprefix",
			rule: "PathPrefix(`/`)",
			want: []Matcher{m("", "/*")},
		},
		{
			name: "same host conjunction intersects",
			rule: "Host(`a.example.com`) && Host(`a.example.com`)",
			want: []Matcher{m("a.example.com", "/*")},
		},
		{
			name: "single quotes",
			rule: "Host('a.example.com')",
			want: []Matcher{m("a.example.com", "/*")},
		},
		{
			name: "and binds tighter than or",
			rule: "Host(`a.example.com`) && PathPrefix(`/x`) || Host(`b.example.com`)",
			want: []Matcher{
				px("a.example.com", "/x"),
				m("b.example.com", "/*"),
			},
		},
		{
			name: "hosts are lowercased",
			rule: "Host(`APP.Example.COM`)",
			want: []Matcher{m("app.example.com", "/*")},
		},
		{
			name: "trailing host dot is preserved",
			rule: "Host(`app.example.com.`)",
			want: []Matcher{m("app.example.com.", "/*")},
		},
		{
			name: "multiple trailing host dots stay",
			rule: "Host(`app.example.com...`)",
			want: []Matcher{m("app.example.com...", "/*")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseRule(tt.rule)
			if err != nil {
				t.Fatalf("ParseRule(%q): %v", tt.rule, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ParseRule(%q) = %+v, want %+v", tt.rule, got, tt.want)
			}
		})
	}
}

func TestParseRuleErrors(t *testing.T) {
	tests := []struct {
		name string
		rule string
		want string // substring of the error
	}{
		{"unsupported matcher", "HostRegexp(`.*`)", "not supported"},
		{"negation", "!Host(`a`)", "negation"},
		{"disjoint hosts", "Host(`a`) && Host(`b`)", "never match"},
		{"two paths", "Path(`/a`) && Path(`/b`)", "multiple path matchers"},
		{"unterminated string", "Host(`a", "unterminated"},
		{"dangling operator", "Host(`a`) &&", "end of rule"},
		{"single ampersand", "Host(`a`) & Host(`b`)", `single "&"`},
		{"empty args", "Host()", "at least one argument"},
		{"path placeholder", "Path(`/foo/{id:[0-9]+}`)", "not a literal path"},
		{"pathprefix placeholder", "PathPrefix(`/foo/{id}`)", "not a literal path"},
		{"header matcher", "Header(`X-Foo`, `bar`)", "not supported"},
		{"trailing garbage", "Host(`a`) Host(`b`)", "unexpected"},
		{"empty rule", "", "end of rule"},
		{"missing comma between args", "Host(`a``b`)", "comma"},
		{"wildcard host", "Host(`*`)", "not a literal host"},
		{"wildcard host suffix", "Host(`*.example.com`)", "not a literal host"},
		{"percent path", "Path(`/a%20b`)", "not a literal path"},
		{"percent pathprefix", "PathPrefix(`/a%20b`)", "not a literal path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseRule(tt.rule)
			if err == nil {
				t.Fatalf("ParseRule(%q): expected error", tt.rule)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ParseRule(%q) error = %q, want substring %q", tt.rule, err, tt.want)
			}
		})
	}
}

// TestParseRuleExpansionCap covers the maxRuleMatchers guard against
// pathological labels inflating the route table.
func TestParseRuleExpansionCap(t *testing.T) {
	hosts := make([]string, maxRuleMatchers+1)
	for i := range hosts {
		hosts[i] = fmt.Sprintf("`h%d.example.com`", i)
	}
	rule := "Host(" + strings.Join(hosts, ",") + ")"
	if _, err := ParseRule(rule); err == nil || !strings.Contains(err.Error(), "matchers") {
		t.Fatalf("oversized host list not capped: %v", err)
	}

	// The cap also applies mid-expansion in andExpr, when a conjunction
	// distributes over disjunctions whose product exceeds it: 9 Host()
	// alternatives × 8 PathPrefix arguments = 72 conjunctions.
	hostAlts := make([]string, 0, 9)
	for i := range 9 {
		hostAlts = append(hostAlts, fmt.Sprintf("Host(`h%d.example.com`)", i))
	}
	prefixes := make([]string, 8)
	for i := range prefixes {
		prefixes[i] = fmt.Sprintf("`/p%d`", i)
	}
	rule = "(" + strings.Join(hostAlts, " || ") + ") && PathPrefix(" + strings.Join(prefixes, ",") + ")"
	if _, err := ParseRule(rule); err == nil || !strings.Contains(err.Error(), "matchers") {
		t.Fatalf("oversized conjunction not capped: %v", err)
	}

	// A rule at the cap still parses.
	rule = "Host(" + strings.Join(hosts[:maxRuleMatchers], ",") + ")"
	got, err := ParseRule(rule)
	if err != nil || len(got) != maxRuleMatchers {
		t.Fatalf("rule at cap failed: %d matchers, err %v", len(got), err)
	}
}

func TestMatcherEqual(t *testing.T) {
	base := Matcher{Host: "a.example.com", HostKind: HostExact, Path: "/api/*", PathKind: PathSegment, Middlewares: []string{"auth", "strip"}}
	cases := []struct {
		name string
		m    Matcher
		want bool
	}{
		{"identical", Matcher{Host: "a.example.com", HostKind: HostExact, Path: "/api/*", PathKind: PathSegment, Middlewares: []string{"auth", "strip"}}, true},
		{"different host", Matcher{Host: "b.example.com", HostKind: HostExact, Path: "/api/*", PathKind: PathSegment, Middlewares: []string{"auth", "strip"}}, false},
		{"different path", Matcher{Host: "a.example.com", HostKind: HostExact, Path: "/*", PathKind: PathAny, Middlewares: []string{"auth", "strip"}}, false},
		{"different host semantics", Matcher{Host: "a.example.com", HostKind: HostTraefik, Path: "/api/*", PathKind: PathSegment, Middlewares: []string{"auth", "strip"}}, false},
		{"different middlewares", Matcher{Host: "a.example.com", HostKind: HostExact, Path: "/api/*", PathKind: PathSegment, Middlewares: []string{"auth"}}, false},
		// Middleware order is semantic: order-only differences are
		// different routes, never silently collapsed.
		{"reordered middlewares", Matcher{Host: "a.example.com", HostKind: HostExact, Path: "/api/*", PathKind: PathSegment, Middlewares: []string{"strip", "auth"}}, false},
	}
	for _, tc := range cases {
		if got := base.Equal(tc.m); got != tc.want {
			t.Errorf("%s: Equal = %v, want %v", tc.name, got, tc.want)
		}
	}
	none := Matcher{Path: "/*"}
	if !none.Equal(Matcher{Path: "/*"}) {
		t.Error("middleware-less matchers unequal")
	}
}
