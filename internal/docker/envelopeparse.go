package docker

import (
	"fmt"
	"strings"
)

// Token kinds for the envelope lexer. They are separate from the strict
// lexer's on purpose: the two readers must be free to diverge, and this one
// already has a token the other refuses to produce.
const (
	envIdent = iota
	envString
	envLParen
	envRParen
	envComma
	envAnd
	envOr
	envNot
)

type envToken struct {
	kind int
	val  string
}

// lexEnvelope tokenizes a rule for the deriver. It differs from lexRule in
// one place: '!' lexes as a token. A negation's siblings still bound the
// rule and lexRule throws them away. Any other unread byte ends the
// derivation; the caller widens to the global envelope.
func lexEnvelope(s string) ([]envToken, error) {
	var toks []envToken
	for i := 0; i < len(s); {
		tok, next, err := nextEnvToken(s, i)
		if err != nil {
			return nil, err
		}
		if tok != nil {
			toks = append(toks, *tok)
		}
		i = next
	}
	return toks, nil
}

// envSingleCharTokens maps the punctuation that lexes as itself.
var envSingleCharTokens = map[byte]envToken{
	'(': {envLParen, "("},
	')': {envRParen, ")"},
	',': {envComma, ","},
	'!': {envNot, "!"},
}

// nextEnvToken lexes the token starting at s[i], returning nil for skipped
// whitespace and the index just past the consumed bytes.
func nextEnvToken(s string, i int) (*envToken, int, error) {
	c := s[i]
	if isRuleSpace(c) {
		return nil, i + 1, nil
	}
	if tok, ok := envSingleCharTokens[c]; ok {
		return &tok, i + 1, nil
	}
	switch {
	case c == '&' || c == '|':
		return lexEnvOperator(s, i)
	case isRuleQuote(c):
		return lexEnvString(s, i)
	case isRuleIdentChar(c):
		return lexEnvIdent(s, i)
	default:
		return nil, 0, fmt.Errorf("envelope: unexpected character %q at offset %d", c, i)
	}
}

func lexEnvIdent(s string, i int) (*envToken, int, error) {
	j := i
	for j < len(s) && isRuleIdentChar(s[j]) {
		j++
	}
	return &envToken{envIdent, s[i:j]}, j, nil
}

func lexEnvOperator(s string, i int) (*envToken, int, error) {
	c := s[i]
	if i+1 >= len(s) || s[i+1] != c {
		return nil, 0, fmt.Errorf("envelope: single %q at offset %d", string(c), i)
	}
	if c == '&' {
		return &envToken{envAnd, "&&"}, i + 2, nil
	}
	return &envToken{envOr, "||"}, i + 2, nil
}

func lexEnvString(s string, i int) (*envToken, int, error) {
	c := s[i]
	end := strings.IndexByte(s[i+1:], c)
	if end < 0 {
		return nil, 0, fmt.Errorf("envelope: unterminated string at offset %d", i)
	}
	return &envToken{envString, s[i+1 : i+1+end]}, i + end + 2, nil
}

// envExpr is a node in the tolerantly parsed rule tree. Its envelope method
// returns the node's disjunctive normal form, plus a bool reporting that the
// node is the top envelope (every request). The flag names a set, and the
// two operators read it differently: a union with top is top, so it
// propagates through envOrExpr, while a meet is a subset of each operand,
// so envAndExpr keeps the bounded operand and only reports top when both
// operands are top.
type envExpr interface {
	envelope() ([]conj, bool)
}

type (
	envOrExpr  struct{ left, right envExpr }
	envAndExpr struct{ left, right envExpr }
	// envNotExpr keeps no operand; see its envelope method.
	envNotExpr struct{}
	envFnExpr  struct {
		name string
		args []string
	}
)

// envTop is the unconstrained envelope: one conjunction with no host and no
// path constraint. It is the identity of the conjunction merge, so dropping
// an unrepresentable conjunct is not a special case.
func envTop() []conj { return []conj{{}} }

// envelope replaces a negation node with the unconstrained envelope. The
// complement of a host is not a host literal and the complement of a prefix
// is not a prefix, so no element of the lattice but the top one contains it.
// Siblings are not poisoned: the substitution sits at positive polarity, so
// the widening it introduces stays monotone.
func (envNotExpr) envelope() ([]conj, bool) { return envTop(), false }

func (e envOrExpr) envelope() ([]conj, bool) {
	l, lg := e.left.envelope()
	r, rg := e.right.envelope()
	if lg || rg {
		return nil, true
	}
	out := make([]conj, 0, len(l)+len(r))
	out = append(out, l...)
	out = append(out, r...)
	return out, false
}

// envelope intersects the two operands. A top operand is not a reason to
// widen the meet: [[A && B]] is contained in [[B]], so when only one side
// is top the other side alone is already a sound superset. Collapsing
// there would refuse every unmatched request in the generation and disable
// Config.Fallback on the strength of one unreadable conjunct.
// The narrowing does not depend on why the flag was raised. Both sources,
// a zero-argument matcher and the working-set cap below, mean the same
// thing: that side bounds nothing.
func (e envAndExpr) envelope() ([]conj, bool) {
	ls, lg := e.left.envelope()
	rs, rg := e.right.envelope()
	switch {
	case lg && rg:
		return nil, true
	case lg:
		return rs, false
	case rg:
		return ls, false
	}
	if len(ls)*len(rs) > envWorkingCap {
		ls, rs = coarsenConjs(ls), coarsenConjs(rs)
		if len(ls)*len(rs) > envWorkingCap {
			return nil, true
		}
	}
	out := make([]conj, 0, len(ls)*len(rs))
	for _, l := range ls {
		for _, r := range rs {
			out = append(out, mergeEnvConj(l, r))
		}
	}
	return out, false
}

// mergeEnvConj intersects two conjunctions the tolerant way, never failing.
// The first host set wins, because the true intersection is a subset of
// each conjunct and the parser's "disjoint hosts can never match" verdict
// is not a proof that the request set is empty: it compares hosts raw while
// the dispatcher compares them case-insensitively. Two path constraints
// keep the enclosing one, or the leftmost when they are incomparable. The
// intersection is a subset of either, so either alone is sound.
func mergeEnvConj(a, b conj) conj {
	out := a
	if len(out.hosts) == 0 {
		out.hosts = b.hosts
	}
	switch {
	case !b.pathSet:
	case !out.pathSet:
		out.path, out.prefix, out.pathSet = b.path, b.prefix, true
	case conjPathLE(out, b):
		out.path, out.prefix = b.path, b.prefix
	}
	return out
}

// conjPathLE reports whether a's path constraint is contained in b's. Both
// conjunctions must carry one.
func conjPathLE(a, b conj) bool {
	if !b.prefix {
		return !a.prefix && a.path == b.path
	}
	bp := strings.TrimSuffix(b.path, "/")
	if bp == "" {
		return true
	}
	ap := a.path
	if a.prefix {
		ap = strings.TrimSuffix(ap, "/")
	}
	return ap == bp || strings.HasPrefix(ap, bp+"/")
}

// envelope reduces one matcher call. Host, Path, and PathPrefix contribute
// their constraint; every other name (ClientIP, Header, Query, Method, the
// regexp matchers, and anything Traefik adds later) contributes nothing
// and reduces to the top envelope. Names are never special-cased and a
// literal is never mined out of a regexp: statute has no wildcard-host
// matcher, and alternation defeats prefix mining even in principle.
//
// A zero-argument call is a matcher statute cannot read at all, so it
// reports top through the flag. As the whole rule that is the global
// tombstone; beside a bounded conjunct the meet keeps the conjunct.
func (e envFnExpr) envelope() ([]conj, bool) {
	if len(e.args) == 0 {
		return nil, true
	}
	switch e.name {
	case "Host":
		return hostLeaf(e.args), false
	case "Path":
		return pathLeaf(e.args, false), false
	case "PathPrefix":
		return pathLeaf(e.args, true), false
	default:
		return envTop(), false
	}
}

// hostLeaf turns Host() arguments into one host set. A multi-argument
// matcher is a disjunction, so a single unread argument leaves the whole
// leaf unconstrained.
func hostLeaf(args []string) []conj {
	for _, a := range args {
		if a == "" || !literalArg(a) {
			return envTop()
		}
	}
	return []conj{{hosts: args}}
}

// pathLeaf turns Path()/PathPrefix() arguments into one conjunction each,
// the same disjunction reading, with the same all-or-nothing literal test.
// An argument that is empty or does not begin with "/" is not a pattern the
// dispatcher can match (req.URL.Path always starts with "/"), so it widens
// like any other unread argument.
func pathLeaf(args []string, prefix bool) []conj {
	out := make([]conj, 0, len(args))
	for _, a := range args {
		if !literalArg(a) || !strings.HasPrefix(a, "/") {
			return envTop()
		}
		out = append(out, conj{path: a, prefix: prefix, pathSet: true})
	}
	return out
}

// envMetaChars are the characters that make an argument something other
// than the literal the dispatcher compares against: Traefik placeholders
// and regexp syntax. '.' is deliberately absent: it is a literal byte in
// both a host and a path, and every host literal contains one.
const envMetaChars = "{}^$*+?()[]|\\"

// literalArg reports whether an argument is a literal the dispatcher will
// compare byte for byte. Placeholder shapes are not truncated to their last
// segment boundary: that needs a proof about a placeholder grammar which
// differs between Traefik v2 and v3.
func literalArg(s string) bool {
	return !strings.ContainsAny(s, envMetaChars)
}

// envParser is a recursive-descent parser over the tolerant token stream.
// Grammar: or := and ("||" and)* ; and := term ("&&" term)* ;
// term := "!" term | "(" or ")" | ident "(" string ("," string)* ")".
type envParser struct {
	toks []envToken
	pos  int
}

func (p *envParser) peek() (envToken, bool) {
	if p.pos >= len(p.toks) {
		return envToken{}, false
	}
	return p.toks[p.pos], true
}

func (p *envParser) parseOr() (envExpr, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for {
		t, ok := p.peek()
		if !ok || t.kind != envOr {
			return left, nil
		}
		p.pos++
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = envOrExpr{left, right}
	}
}

func (p *envParser) parseAnd() (envExpr, error) {
	left, err := p.parseTerm()
	if err != nil {
		return nil, err
	}
	for {
		t, ok := p.peek()
		if !ok || t.kind != envAnd {
			return left, nil
		}
		p.pos++
		right, err := p.parseTerm()
		if err != nil {
			return nil, err
		}
		left = envAndExpr{left, right}
	}
}

func (p *envParser) parseTerm() (envExpr, error) {
	t, ok := p.peek()
	if !ok {
		return nil, fmt.Errorf("envelope: unexpected end of rule")
	}
	switch t.kind {
	case envNot:
		p.pos++
		// The operand is parsed to keep the stream in step, then
		// discarded: the node itself becomes the top envelope.
		if _, err := p.parseTerm(); err != nil {
			return nil, err
		}
		return envNotExpr{}, nil
	case envLParen:
		p.pos++
		inner, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if err := p.expect(envRParen, ")"); err != nil {
			return nil, err
		}
		return inner, nil
	case envIdent:
		return p.parseCall(t.val)
	default:
		return nil, fmt.Errorf("envelope: expected matcher, got %q", t.val)
	}
}

// parseCall consumes a matcher call whose name token is still current.
func (p *envParser) parseCall(name string) (envExpr, error) {
	p.pos++
	if err := p.expect(envLParen, "("); err != nil {
		return nil, err
	}
	args, err := p.parseArgs()
	if err != nil {
		return nil, err
	}
	return envFnExpr{name: name, args: args}, nil
}

// parseArgs consumes an argument list up to and including its closing
// parenthesis.
func (p *envParser) parseArgs() ([]string, error) {
	var args []string
	for {
		at, ok := p.peek()
		if !ok {
			return nil, fmt.Errorf("envelope: unterminated argument list")
		}
		switch at.kind {
		case envString:
			args = append(args, at.val)
			p.pos++
			if ct, ok := p.peek(); ok && ct.kind == envComma {
				p.pos++
			}
		case envRParen:
			p.pos++
			return args, nil
		default:
			return nil, fmt.Errorf("envelope: unexpected %q in argument list", at.val)
		}
	}
}

func (p *envParser) expect(kind int, what string) error {
	t, ok := p.peek()
	if !ok || t.kind != kind {
		return fmt.Errorf("envelope: expected %q", what)
	}
	p.pos++
	return nil
}
