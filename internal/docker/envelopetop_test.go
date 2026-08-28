package docker

import "testing"

// topBases are rules both readers accept: ParseRule for the strict one, and
// the deriver for the envelope each identity below is stated against.
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

// A zero-argument matcher is the top envelope, and the two operators must
// read that one fact in opposite directions. Over every base B in the table,
// with T a zero-argument call:
//
//	B && T == B      T && B == B
//	B || T == top    T || B == top
//
// Equality is the claim. Containment is what the tier's invariant demands
// and what FuzzRuleEnvelope checks, but collapsing the meet back to the
// global envelope still covers every request B covers, so a containment
// assertion passes for the widened answer and for the narrowed one.
// Comparing against B's own envelope separates "kept the bounded operand"
// from "refused every request in the generation".
//
// FuzzRuleEnvelope cannot reach these shapes either. It only compares rules
// ParseRule accepts, ParseRule rejects every zero-argument call, and the
// working-set cap that also raises the flag sits far above ParseRule's own
// matcher budget, so no accepted rule raises it anywhere in its tree.
//
// Two bases derive the global envelope legitimately (PathPrefix(`/`) bounds
// nothing, and a percent-escaped literal widens), so a base deriving it is
// not itself a failure. Every base deriving it would be: the identities would
// then hold for a deriver that widened everything. The count below refuses
// a table where none of them is bounded.
func TestRuleEnvelopeZeroArgMatcherIsTop(t *testing.T) {
	t.Parallel()
	bounded := 0
	for _, base := range topBases {
		if _, err := ParseRule(base); err != nil {
			t.Fatalf("base %q rejected: %v; every base here must be a rule both readers accept", base, err)
		}
		want := RuleEnvelope(base)
		if len(want) == 0 {
			t.Fatalf("RuleEnvelope(%q) returned no envelope for a base rule", base)
		}
		if !matchersEqual(want, global()) {
			bounded++
		}
		for _, z := range zeroArgMatchers {
			assertMeetIsBase(t, "("+base+") && "+z, base, want)
			assertMeetIsBase(t, z+" && ("+base+")", base, want)
			assertUnionIsGlobal(t, "("+base+") || "+z)
			assertUnionIsGlobal(t, z+" || ("+base+")")
		}
	}
	// A deriver that widened everything to global satisfies both identities
	// above, so at least one base must derive a bounded envelope.
	if bounded == 0 {
		t.Fatal("no base derived a bounded envelope; the identities above would hold vacuously")
	}
}

// assertMeetIsBase checks that the readable operand survives a meet with a
// top operand unchanged. want is the base's own envelope, so the table
// states the identity without a second copy of TestRuleEnvelope's pins.
// Comparison is elementwise because EnvelopeOf sorts and absorbs, so one
// request set has one representation here.
func assertMeetIsBase(t *testing.T, rule, base string, want []Matcher) {
	t.Helper()
	if got := RuleEnvelope(rule); !matchersEqual(got, want) {
		t.Errorf("RuleEnvelope(%q) = %v, want RuleEnvelope(%q) = %v: a meet is contained in each operand, so an unreadable conjunct is dropped rather than propagated", rule, got, base, want)
	}
}

// assertUnionIsGlobal checks that a top branch widens the whole disjunction.
func assertUnionIsGlobal(t *testing.T, rule string) {
	t.Helper()
	if got := RuleEnvelope(rule); !matchersEqual(got, global()) {
		t.Errorf("RuleEnvelope(%q) = %v, want the global envelope: a branch that bounds nothing bounds the union", rule, got)
	}
}
