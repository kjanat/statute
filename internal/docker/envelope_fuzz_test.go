package docker

import (
	"strings"
	"testing"
)

// matchesRequest uses the same Matcher.Match the dispatcher compiles onto
// compiledRoute. Duplicated here only as a name the fuzz target already uses.
func matchesRequest(mt Matcher, host, path string) bool {
	return mt.Match(host, path)
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
// its host in the spellings Traefik folds together, and the paths at and
// below its pattern, including the byte-prefix hole past a segment boundary.
func probeRequests(mt Matcher) [][2]string {
	hosts := hostWitnesses(mt)
	paths := []string{"/", "/x", "/api", "/api/v1/keys", "/login"}
	switch mt.PathKind {
	case PathByte:
		paths = append(paths, mt.Path, mt.Path+"/", mt.Path+"/deep/leaf", mt.Path+"foo")
	case PathSegment:
		before, _ := strings.CutSuffix(mt.Path, "/*")
		paths = append(paths, before, before+"/", before+"/deep/leaf", before+"foo")
	default:
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
		"Host(`0.`, `0`)",
		"Host(`0..`, `0`)",
		"Host(`0..``0`)",
		"Host(`*`)",
		"Host(`*.example.com`)",
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
				if !matchesAny(env, host, path) {
					t.Fatalf("rule %q: request %q %q matches route %+v but no envelope element in %v", rule, host, path, mt, env)
				}
			}
		}
	})
}
