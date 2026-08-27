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
// allowed; refusing less is not, because the traffic that escapes reaches
// the operator's Config.Fallback instead of the 404 a dropped router used
// to produce.
//
// It is a second, deliberately tolerant entry point beside ParseRule.
// ParseRule stops at the first construct it cannot represent, and every
// such stop destroys the sibling constraints that were the only thing
// bounding the rule. RuleEnvelope instead widens: an unrepresentable
// conjunct is dropped, which can only add requests; a disjunction is a
// branch-aware union, so one unreadable branch widens the whole rule; and a
// construct it cannot bound at all collapses to the global "any host, /*"
// envelope. It never returns a subset.
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
// requests is precisely when the envelope must widen rather than disappear.
func GlobalEnvelope() []Matcher {
	return []Matcher{{Path: "/*"}}
}

// EnvelopeOf widens matchers that were parsed successfully into a sound
// tombstone set. The constraints are kept as they are — the router claimed
// exactly this traffic — but the literals statute's dispatcher cannot
// compare faithfully still widen, and redundant elements are absorbed.
func EnvelopeOf(ms []Matcher) []Matcher {
	return absorbMatchers(normalizeEnvelope(ms))
}

// envWorkingCap bounds the disjunctive expansion the deriver holds in
// memory. It is far above maxRuleMatchers so the coarsening ladder, not
// this cap, decides the shape of an oversized envelope.
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
// is a widening: the paths go first so a host-scoped envelope survives, and
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
// Dropping a path can only add requests, and it is the step that keeps a
// host-scoped envelope when the full expansion does not fit.
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
// faithfully. Host comparison is EqualFold over the port-stripped Host
// header, which is neither IDN folding nor Traefik's trailing-dot
// stripping; patterns are matched against the percent-decoded path, so a
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
		m.Host = envelopeHost(m.Host)
		// The trailing "/*" is the dispatcher's own wildcard rather than
		// a literal, so it is cut before the literal is tested.
		if lit, _ := strings.CutSuffix(m.Path, "/*"); strings.ContainsAny(lit, envMetaChars) || strings.Contains(lit, "%") {
			m.Path = "/*"
		}
		m.Middlewares = nil
		out = append(out, m)
	}
	return out
}

// envelopeHost canonicalizes a host literal for the tombstone tier. The
// tier strips a trailing FQDN dot from the request host, so the literal
// loses one too; a non-ASCII literal widens to any host.
func envelopeHost(h string) string {
	h = strings.ToLower(strings.TrimSuffix(h, "."))
	if !isASCII(h) {
		return ""
	}
	return h
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
		return strings.Compare(a.Path, b.Path)
	})
}

// matcherLE reports whether every request a matches is also matched by b,
// in the dispatcher's own semantics: an empty host is any host, and a
// trailing "/*" is a segment-aware prefix.
func matcherLE(a, b Matcher) bool {
	if b.Host != "" && a.Host != b.Host {
		return false
	}
	return patternLE(a.Path, b.Path)
}

// patternLE mirrors matchPattern's containment: "/*" covers everything, a
// "/x/*" prefix covers "/x" and anything below "/x/", and anything else is
// byte equality.
func patternLE(a, b string) bool {
	bp, wildcard := strings.CutSuffix(b, "/*")
	if !wildcard {
		return a == b
	}
	if bp == "" {
		return true
	}
	ap, _ := strings.CutSuffix(a, "/*")
	return ap == bp || strings.HasPrefix(ap, bp+"/")
}
