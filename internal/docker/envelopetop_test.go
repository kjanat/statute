package docker

import (
	"strings"
	"testing"
)

// topBases are rules ParseRule accepts, so their matchers give an oracle for
// the request set a zero-argument sibling must not shrink.
var topBases = []string{
	"Host(`a.example.com`)",
	"Path(`/login`)",
	"PathPrefix(`/api`)",
	"PathPrefix(`/`)",
	"Host(`app.example.com`) && PathPrefix(`/api`)",
	"Host(`a.example.com`) || Host(`b.example.com`)",
	"Host(`a.example.com`, `b.example.com`) && PathPrefix(`/api`)",
	"Host(`a.example.com.`)",
	"Path(`/a%20b`)",
	"(Host(`a.example.com`) || Host(`b.example.com`)) && PathPrefix(`/x`)",
}

// zeroArgMatchers bound nothing whatever the name in front of them is.
var zeroArgMatchers = []string{"Path()", "Host()", "PathPrefix()", "ClientIP()", "Foo()"}

// TestRuleEnvelopeZeroArgMatcherIsTop — a zero-argument matcher is the top
// envelope, and the two operators must read that one fact in opposite
// directions: a meet is contained in each operand, so the readable operand
// survives; a union contains each operand, so a top branch poisons it.
//
// FuzzRuleEnvelope cannot reach either. It only compares rules ParseRule
// accepts, ParseRule rejects every zero-argument call, and the working-set
// cap that also raises the flag sits far above ParseRule's own matcher
// budget — so no accepted rule raises it anywhere in its tree. Containment
// alone would not settle it either: over-refusing satisfies the invariant,
// so widening back to global passes every containment check ever written.
// This is the guard for the pair over more shapes than one fixed table.
func TestRuleEnvelopeZeroArgMatcherIsTop(t *testing.T) {
	t.Parallel()
	for _, base := range topBases {
		routes, err := ParseRule(base)
		if err != nil {
			t.Fatalf("base %q rejected: %v; this table needs an oracle", base, err)
		}
		for _, z := range zeroArgMatchers {
			assertMeetKeepsBase(t, "("+base+") && "+z, routes)
			assertMeetKeepsBase(t, z+" && ("+base+")", routes)
			assertUnionIsGlobal(t, "("+base+") || "+z)
			assertUnionIsGlobal(t, z+" || ("+base+")")
		}
	}
}

// assertMeetKeepsBase checks that the envelope of a conjunction still covers
// every request the readable operand alone would have matched.
func assertMeetKeepsBase(t *testing.T, rule string, routes []Matcher) {
	t.Helper()
	env := RuleEnvelope(rule)
	if len(env) == 0 {
		t.Fatalf("RuleEnvelope(%q) returned no envelope", rule)
	}
	for _, mt := range routes {
		for _, req := range probeRequests(mt) {
			host, path := req[0], req[1]
			if !matchesRequest(mt, host, path) {
				continue
			}
			if !matchesAny(env, strings.TrimSuffix(host, "."), path) {
				t.Fatalf("rule %q: request %q %q matches route %+v but no element of %v", rule, host, path, mt, env)
			}
		}
	}
}

// assertUnionIsGlobal checks that a top branch widens the whole disjunction.
func assertUnionIsGlobal(t *testing.T, rule string) {
	t.Helper()
	if got := RuleEnvelope(rule); !matchersEqual(got, global()) {
		t.Errorf("RuleEnvelope(%q) = %v, want the global envelope: a branch that bounds nothing bounds the union", rule, got)
	}
}
