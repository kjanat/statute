package docker

import (
	"slices"
	"strings"
	"unicode/utf8"
)

// RuleEnvelope derives the refusal envelope of a Traefik router rule whose
// routes are being discarded: a matcher set guaranteed to cover every
// request the rule itself would have matched.
//
// INVARIANT: for every request q, matches(rule, q) implies that some
// returned matcher matches q. Refusing more than the rule claimed is
// allowed; refusing less is not. Traffic that escapes reaches
// Config.Fallback; a dropped router used to produce 404.
//
// It is a second, deliberately tolerant entry point beside ParseRule.
// ParseRule stops at the first construct it cannot represent, and every
// such stop destroys the sibling constraints that were the only thing
// bounding the rule. RuleEnvelope widens: an unrepresentable conjunct
// is dropped, which can only add requests, and a conjunct that bounds
// nothing at all is dropped the same way, since a conjunction is
// contained in each of its operands. A disjunction is a branch-aware
// union, so one unreadable branch widens the whole rule. Only a rule
// nothing bounds (it does not lex or parse, or every operand widened
// away) collapses to the global "any host, /*" envelope. It never
// returns a subset.
//
// A blank rule is the one empty answer. Traefik matches nothing without a
// rule, so the request set is empty and any envelope, including none,
// satisfies the invariant. The trim is this function's own: the label layer
// tests the raw string, so a whitespace-only rule arrives here.
func RuleEnvelope(rule string) []Matcher {
	if strings.TrimSpace(rule) == "" {
		return nil
	}
	conjs, ok := deriveEnvelope(rule)
	if !ok {
		return GlobalEnvelope()
	}
	return renderEnvelope(conjs)
}

// GlobalEnvelope is the envelope for a rule whose traffic cannot be
// identified at all: any host, every path. Inability to name the affected
// requests is when the envelope must widen.
func GlobalEnvelope() []Matcher {
	return []Matcher{{Path: "/*"}}
}

// EnvelopeOf widens matchers that were parsed successfully into a sound
// tombstone set. The router claimed this traffic, but the literals
// statute's dispatcher cannot compare faithfully still widen, and
// redundant elements are absorbed.
func EnvelopeOf(ms []Matcher) []Matcher {
	return absorbMatchers(normalizeEnvelope(ms))
}

// envWorkingCap bounds the disjunctive expansion the deriver holds in
// memory. It is far above maxRuleMatchers so the coarsening ladder
// decides the shape of an oversized envelope.
const envWorkingCap = 4096

// deriveEnvelope lexes, parses, and expands a rule with the tolerant
// deriver. A false second result means nothing structural survived, which
// is the global envelope.
func deriveEnvelope(rule string) ([]conj, bool) {
	toks, err := lexEnvelope(rule)
	if err != nil {
		return nil, false
	}
	p := &envParser{toks: toks}
	expr, err := p.parseOr()
	if err != nil || p.pos != len(p.toks) {
		return nil, false
	}
	conjs, global := expr.envelope()
	if global || len(conjs) == 0 {
		return nil, false
	}
	return conjs, true
}

// renderEnvelope turns derived conjunctions into matchers, coarsening when
// the expansion does not fit the matcher budget. Every rung of the ladder
// is a widening: the paths go first, a host-scoped envelope survives, and
// only a host set that cannot be reduced reaches the global tombstone.
func renderEnvelope(conjs []conj) []Matcher {
	for _, cs := range [][]conj{conjs, coarsenConjs(conjs)} {
		ms, err := conjsToMatchers(cs)
		if err != nil {
			continue
		}
		return EnvelopeOf(ms)
	}
	return GlobalEnvelope()
}

// coarsenConjs widens every conjunction to its host constraint alone.
// Dropping a path can only add requests. This step preserves a host-scoped
// envelope when the full expansion does not fit.
func coarsenConjs(conjs []conj) []conj {
	var out []conj
	for _, c := range conjs {
		c.path, c.prefix, c.pathSet = "", false, false
		if !slices.ContainsFunc(out, func(o conj) bool { return slices.Equal(o.hosts, c.hosts) }) {
			out = append(out, c)
		}
	}
	return out
}

// normalizeEnvelope widens the literals statute's dispatcher cannot compare
// faithfully. Traefik-derived hosts were canonicalized once while rendering
// the matcher; this stage only widens a non-ASCII host it cannot compare
// faithfully. Native statute hosts remain exact. Patterns are matched against
// the percent-decoded path, so a
// literal carrying an escape matches nothing a client can send while the
// client's decoded path misses the tombstone. A placeholder or regexp
// literal is the same failure by another route: matchPattern compares
// "/api/{id:[0-9]+}" byte for byte, so it refuses nothing.
//
// The widening has to happen here and not only in the deriver, because
// which entry point reads a rule is decided by the stage the rejection
// fired at: a rule ParseRule accepts and an unregistered middleware
// reference then discards arrives as parsed matchers. One rule must not
// produce two different envelopes depending on that.
func normalizeEnvelope(ms []Matcher) []Matcher {
	out := make([]Matcher, 0, len(ms))
	for _, m := range ms {
		if m.FoldHostDot && !isASCII(m.Host) {
			m.Host = ""
		}
		if m.Host == "" {
			m.FoldHostDot = false
		}
		// The trailing "/*" is the dispatcher's wildcard, cut before
		// the literal is tested.
		if lit, _ := strings.CutSuffix(m.Path, "/*"); strings.ContainsAny(lit, envMetaChars) || strings.Contains(lit, "%") {
			m.Path = "/*"
			m.BytePrefix = false
		}
		m.Middlewares = nil
		out = append(out, m)
	}
	return out
}

func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// absorbMatchers drops every element another element already covers, so an
// envelope carries no redundant refusal and collapses to the single global
// tombstone whenever one is present. The result is sorted, so identical
// labels always yield an identical envelope.
func absorbMatchers(ms []Matcher) []Matcher {
	ms = slices.Clone(ms)
	sortMatchers(ms)
	var out []Matcher
	for i, m := range ms {
		if !coveredByOther(ms, i) {
			out = append(out, m)
		}
	}
	return out
}

// coveredByOther reports whether some other element of ms covers ms[i].
// Mutually covering duplicates keep the lowest index only.
func coveredByOther(ms []Matcher, i int) bool {
	for j, o := range ms {
		if i == j || !matcherLE(ms[i], o) {
			continue
		}
		if !matcherLE(o, ms[i]) || j < i {
			return true
		}
	}
	return false
}

func sortMatchers(ms []Matcher) {
	slices.SortStableFunc(ms, func(a, b Matcher) int {
		if a.Host != b.Host {
			return strings.Compare(a.Host, b.Host)
		}
		if a.FoldHostDot != b.FoldHostDot {
			if a.FoldHostDot {
				return 1
			}
			return -1
		}
		if a.BytePrefix != b.BytePrefix {
			if a.BytePrefix {
				return 1
			}
			return -1
		}
		return strings.Compare(a.Path, b.Path)
	})
}

// matcherLE reports whether every request a matches is also matched by b.
// An empty host is any host. PathPrefix is Traefik HasPrefix; a trailing
// "/*" is statute's segment-aware prefix.
func matcherLE(a, b Matcher) bool {
	if !hostMatcherLE(a, b) {
		return false
	}
	if b.BytePrefix {
		return bytePrefixCovers(a, b.Path)
	}
	return statutePatternCovers(a, b.Path)
}

// hostMatcherLE reports whether every host accepted by a is accepted by b.
// A Traefik host covers both its dotted and undotted FQDN spellings. A
// native host is one exact spelling, so containment is directional when
// the two matcher kinds are mixed.
func hostMatcherLE(a, b Matcher) bool {
	if b.Host == "" {
		return true
	}
	if a.Host == "" {
		return false
	}
	if b.FoldHostDot {
		return strings.EqualFold(strings.TrimSuffix(a.Host, "."), b.Host)
	}
	return !a.FoldHostDot && strings.EqualFold(a.Host, b.Host)
}

// bytePrefixCovers reports that every path a matches starts with prefix.
func bytePrefixCovers(a Matcher, prefix string) bool {
	if prefix == "/" {
		return true
	}
	if a.BytePrefix {
		return strings.HasPrefix(a.Path, prefix)
	}
	if before, ok := strings.CutSuffix(a.Path, "/*"); ok {
		if before == "" {
			return false
		}
		return strings.HasPrefix(before, prefix)
	}
	return strings.HasPrefix(a.Path, prefix)
}

// statutePatternCovers reports that every path a matches is matched by
// statute pattern b (exact or trailing "/*").
func statutePatternCovers(a Matcher, b string) bool {
	bp, wildcard := strings.CutSuffix(b, "/*")
	if !wildcard {
		if a.BytePrefix {
			return false
		}
		return a.Path == b
	}
	if bp == "" {
		return true
	}
	if a.BytePrefix {
		// PathPrefix(`/api`) also matches `/api-secret`. Equality with the
		// segment base leaves that path uncovered.
		return strings.HasPrefix(a.Path, bp+"/")
	}
	ap, _ := strings.CutSuffix(a.Path, "/*")
	return ap == bp || strings.HasPrefix(ap, bp+"/")
}
