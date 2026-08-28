package docker

import (
	"strings"
	"testing"
)

// matchesRequest mirrors statute's dispatcher: an empty host matches any
// host and is otherwise compared case-insensitively, and a trailing "/*" is
// a segment-aware prefix over the percent-decoded path. It is duplicated
// here because internal/docker cannot import the server package; the
// duplication is what makes the property testable at all.
func matchesRequest(mt Matcher, host, path string) bool {
	if mt.Host != "" && !strings.EqualFold(mt.Host, host) {
		return false
	}
	if before, ok := strings.CutSuffix(mt.Path, "/*"); ok {
		return before == "" || path == before || strings.HasPrefix(path, before+"/")
	}
	return mt.Path == path
}

// matchesAny reports whether any matcher in the set matches the request.
func matchesAny(ms []Matcher, host, path string) bool {
	for _, mt := range ms {
		if matchesRequest(mt, host, path) {
			return true
		}
	}
	return false
}

// probeRequests builds requests that exercise a matcher's own boundaries:
// its host in several spellings the dispatcher folds together, and the
// paths at and below its pattern.
func probeRequests(mt Matcher) [][2]string {
	hosts := []string{"", "other.example.com", "a.example.com"}
	if mt.Host != "" {
		hosts = append(hosts, mt.Host, strings.ToUpper(mt.Host), mt.Host+".")
	}
	paths := []string{"/", "/x", "/api", "/api/v1/keys", "/login"}
	if before, ok := strings.CutSuffix(mt.Path, "/*"); ok {
		paths = append(paths, before, before+"/", before+"/deep/leaf", before+"foo")
	} else {
		paths = append(paths, mt.Path, mt.Path+"/x")
	}
	out := make([][2]string, 0, len(hosts)*len(paths))
	for _, h := range hosts {
		for _, p := range paths {
			out = append(out, [2]string{h, p})
		}
	}
	return out
}

// FuzzRuleEnvelope invariants: RuleEnvelope never panics; it returns an
// envelope for every rule that is not blank; and for every rule ParseRule
// accepts, every request one of its matchers would have matched is also
// matched by some envelope element. That containment property is the
// mechanical guard against the strict and tolerant readers drifting apart.
//
// Rules ParseRule rejects have no output to compare against, so their
// shapes are pinned by the table in TestRuleEnvelope.
func FuzzRuleEnvelope(f *testing.F) {
	seeds := []string{
		"Host(`app.example.com`)",
		"Path(`/login`)",
		"PathPrefix(`/api`)",
		"PathPrefix(`/`)",
		"Host(`app.example.com`) && PathPrefix(`/api`)",
		"Host(`a.example.com`) || Host(`b.example.com`)",
		"Host(`a.example.com`, `b.example.com`) && PathPrefix(`/api`)",
		"(Host(`a.example.com`) || PathPrefix(`/x`)) && ClientIP(`10.0.0.0/8`)",
		"Host(`a.example.com`) && !PathPrefix(`/public`)",
		"Host(`a.example.com`) && Host(`b.example.com`)",
		"Path(`/a`) && Path(`/b`)",
		"HostRegexp(`^a.*`) && PathPrefix(`/api`)",
		"Host(`a.example.com.`)",
		"Path(`/a%20b`)",
		"Host(`a` && Path(`/x`)",
		"Host()",
		"",
		"   ",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, rule string) {
		env := RuleEnvelope(rule)
		if strings.TrimSpace(rule) == "" {
			if len(env) != 0 {
				t.Errorf("RuleEnvelope(%q) = %v, want no tombstone for a blank rule", rule, env)
			}
			return
		}
		if len(env) == 0 {
			t.Fatalf("RuleEnvelope(%q) returned no envelope; a rule that cannot be bounded must widen, not disappear", rule)
		}
		routes, err := ParseRule(rule)
		if err != nil {
			return
		}
		for _, mt := range routes {
			for _, req := range probeRequests(mt) {
				host, path := req[0], req[1]
				if !matchesRequest(mt, host, path) {
					continue
				}
				if !matchesAny(env, strings.TrimSuffix(host, "."), path) {
					t.Fatalf("rule %q: request %q %q matches route %+v but no envelope element in %v", rule, host, path, mt, env)
				}
			}
		}
	})
}
