package docker

import (
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
			want: []Matcher{{Host: "app.example.com", Path: "/*"}},
		},
		{
			name: "host and pathprefix",
			rule: "Host(`app.example.com`) && PathPrefix(`/api`)",
			want: []Matcher{{Host: "app.example.com", Path: "/api/*"}},
		},
		{
			name: "exact path",
			rule: "Path(`/login`)",
			want: []Matcher{{Path: "/login"}},
		},
		{
			name: "multi-arg host is a disjunction",
			rule: "Host(`a.example.com`, `b.example.com`)",
			want: []Matcher{
				{Host: "a.example.com", Path: "/*"},
				{Host: "b.example.com", Path: "/*"},
			},
		},
		{
			name: "or of hosts",
			rule: "Host(`a.example.com`) || Host(`b.example.com`)",
			want: []Matcher{
				{Host: "a.example.com", Path: "/*"},
				{Host: "b.example.com", Path: "/*"},
			},
		},
		{
			name: "and distributes over or",
			rule: "(Host(`a.example.com`) || Host(`b.example.com`)) && PathPrefix(`/api`)",
			want: []Matcher{
				{Host: "a.example.com", Path: "/api/*"},
				{Host: "b.example.com", Path: "/api/*"},
			},
		},
		{
			name: "double quotes",
			rule: `Host("app.example.com")`,
			want: []Matcher{{Host: "app.example.com", Path: "/*"}},
		},
		{
			name: "pathprefix with trailing slash normalizes",
			rule: "PathPrefix(`/api/`)",
			want: []Matcher{{Path: "/api/*"}},
		},
		{
			name: "root pathprefix",
			rule: "PathPrefix(`/`)",
			want: []Matcher{{Path: "/*"}},
		},
		{
			name: "same host conjunction intersects",
			rule: "Host(`a.example.com`) && Host(`a.example.com`)",
			want: []Matcher{{Host: "a.example.com", Path: "/*"}},
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
		{"header matcher", "Header(`X-Foo`, `bar`)", "not supported"},
		{"trailing garbage", "Host(`a`) Host(`b`)", "unexpected"},
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
