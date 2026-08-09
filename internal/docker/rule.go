package docker

import (
	"fmt"
	"strings"
)

// Matcher is one flattened route matcher produced from a Traefik router
// rule: an optional host plus a statute path pattern. A rule expands to
// one or more matchers (disjunctions become separate matchers).
type Matcher struct {
	Host string
	// Path is a statute pattern: exact ("/login") or trailing-wildcard
	// prefix ("/api/*"). Defaults to "/*" when the rule constrains only
	// the host.
	Path string
}

// maxRuleMatchers caps the disjunctive expansion of a single rule so a
// pathological label cannot balloon the route table.
const maxRuleMatchers = 64

// ParseRule parses a Traefik v2/v3 router rule into statute matchers.
//
// Supported: Host(`a`), Path(`/x`), PathPrefix(`/x`), the `&&` and `||`
// operators, parentheses, and multi-argument matchers (Host(`a`,`b`) is a
// disjunction, per Traefik v2). Unsupported matchers (HostRegexp, Query,
// Header, ClientIP, negation, …) return an error so the caller can skip
// the router with a warning instead of silently mis-routing.
func ParseRule(rule string) ([]Matcher, error) {
	toks, err := lexRule(rule)
	if err != nil {
		return nil, err
	}
	p := &ruleParser{toks: toks}
	expr, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.toks) {
		return nil, fmt.Errorf("unexpected %q", p.toks[p.pos].val)
	}
	conjs, err := expr.expand()
	if err != nil {
		return nil, err
	}
	return conjsToMatchers(conjs)
}

// conjsToMatchers flattens conjunctions into concrete matchers, expanding
// per-conjunction host disjunctions into one matcher per host.
func conjsToMatchers(conjs []conj) ([]Matcher, error) {
	var out []Matcher
	for _, c := range conjs {
		path := "/*"
		if c.pathSet {
			path = c.path
			if c.prefix {
				path = strings.TrimSuffix(path, "/") + "/*"
			}
		}
		hosts := c.hosts
		if len(hosts) == 0 {
			hosts = []string{""}
		}
		for _, h := range hosts {
			out = append(out, Matcher{Host: h, Path: path})
			if len(out) > maxRuleMatchers {
				return nil, fmt.Errorf("rule expands to more than %d matchers", maxRuleMatchers)
			}
		}
	}
	return out, nil
}

// token kinds for the rule lexer.
const (
	tokIdent = iota
	tokString
	tokLParen
	tokRParen
	tokComma
	tokAnd
	tokOr
)

type ruleToken struct {
	kind int
	val  string
}

// lexRule tokenizes a rule string. Matcher arguments may be quoted with
// backticks, double quotes, or single quotes.
func lexRule(s string) ([]ruleToken, error) {
	var toks []ruleToken
	for i := 0; i < len(s); {
		tok, next, err := nextRuleToken(s, i)
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

// singleCharTokens maps the punctuation characters that lex as themselves.
var singleCharTokens = map[byte]ruleToken{
	'(': {tokLParen, "("},
	')': {tokRParen, ")"},
	',': {tokComma, ","},
}

// nextRuleToken lexes the single token starting at s[i], returning nil for
// skipped whitespace, and the index just past the consumed bytes.
func nextRuleToken(s string, i int) (*ruleToken, int, error) {
	c := s[i]
	if isRuleSpace(c) {
		return nil, i + 1, nil
	}
	if tok, ok := singleCharTokens[c]; ok {
		return &tok, i + 1, nil
	}
	switch {
	case c == '&' || c == '|':
		return lexRuleOperator(s, i)
	case isRuleQuote(c):
		return lexRuleString(s, i)
	case isRuleIdentChar(c):
		return lexRuleIdent(s, i)
	case c == '!':
		return nil, 0, fmt.Errorf("rule: negation ('!') is not supported")
	default:
		return nil, 0, fmt.Errorf("rule: unexpected character %q at offset %d", c, i)
	}
}

// lexRuleIdent lexes a matcher name.
func lexRuleIdent(s string, i int) (*ruleToken, int, error) {
	j := i
	for j < len(s) && isRuleIdentChar(s[j]) {
		j++
	}
	return &ruleToken{tokIdent, s[i:j]}, j, nil
}

// lexRuleOperator lexes '&&' or '||', rejecting the single-character forms.
func lexRuleOperator(s string, i int) (*ruleToken, int, error) {
	c := s[i]
	if i+1 >= len(s) || s[i+1] != c {
		return nil, 0, fmt.Errorf("rule: single %q at offset %d", string(c), i)
	}
	if c == '&' {
		return &ruleToken{tokAnd, "&&"}, i + 2, nil
	}
	return &ruleToken{tokOr, "||"}, i + 2, nil
}

// lexRuleString lexes a quoted argument; the closing quote must match the
// opening one.
func lexRuleString(s string, i int) (*ruleToken, int, error) {
	c := s[i]
	end := strings.IndexByte(s[i+1:], c)
	if end < 0 {
		return nil, 0, fmt.Errorf("rule: unterminated string at offset %d", i)
	}
	return &ruleToken{tokString, s[i+1 : i+1+end]}, i + end + 2, nil
}

func isRuleSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func isRuleQuote(c byte) bool {
	return c == '`' || c == '"' || c == '\''
}

func isRuleIdentChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// ruleExpr is a node in the parsed rule tree.
type ruleExpr interface {
	// expand returns the disjunctive normal form of the expression: a
	// list of conjunctions, any of which matching means the rule matches.
	expand() ([]conj, error)
}

// conj is a single conjunction: hosts is an "any of" set (empty = any
// host), and at most one path constraint.
type conj struct {
	hosts   []string
	path    string
	prefix  bool
	pathSet bool
}

type (
	orExpr  struct{ left, right ruleExpr }
	andExpr struct{ left, right ruleExpr }
)

// fnExpr is a leaf matcher call, e.g. Host(`a`, `b`).
type fnExpr struct {
	name string
	args []string
}

func (e orExpr) expand() ([]conj, error) {
	l, err := e.left.expand()
	if err != nil {
		return nil, err
	}
	r, err := e.right.expand()
	if err != nil {
		return nil, err
	}
	return append(l, r...), nil
}

func (e andExpr) expand() ([]conj, error) {
	ls, err := e.left.expand()
	if err != nil {
		return nil, err
	}
	rs, err := e.right.expand()
	if err != nil {
		return nil, err
	}
	var out []conj
	for _, l := range ls {
		for _, r := range rs {
			m, err := mergeConj(l, r)
			if err != nil {
				return nil, err
			}
			out = append(out, m)
			if len(out) > maxRuleMatchers {
				return nil, fmt.Errorf("rule expands to more than %d matchers", maxRuleMatchers)
			}
		}
	}
	return out, nil
}

// mergeConj intersects two conjunctions. Host sets intersect; two path
// constraints on one conjunction are rejected.
func mergeConj(a, b conj) (conj, error) {
	out := a
	if len(b.hosts) > 0 {
		if len(out.hosts) == 0 {
			out.hosts = b.hosts
		} else {
			var inter []string
			for _, h := range out.hosts {
				for _, g := range b.hosts {
					if h == g {
						inter = append(inter, h)
					}
				}
			}
			if len(inter) == 0 {
				return conj{}, fmt.Errorf("rule: conjunction of disjoint Host() sets can never match")
			}
			out.hosts = inter
		}
	}
	if b.pathSet {
		if out.pathSet {
			return conj{}, fmt.Errorf("rule: multiple path matchers in one conjunction are not supported")
		}
		out.path, out.prefix, out.pathSet = b.path, b.prefix, true
	}
	return out, nil
}

func (e fnExpr) expand() ([]conj, error) {
	if len(e.args) == 0 {
		return nil, fmt.Errorf("rule: %s() requires at least one argument", e.name)
	}
	switch e.name {
	case "Host":
		return []conj{{hosts: e.args}}, nil
	case "Path":
		if len(e.args) > 1 {
			// Multi-arg Path is a disjunction (Traefik v2).
			var out []conj
			for _, a := range e.args {
				out = append(out, conj{path: a, pathSet: true})
			}
			return out, nil
		}
		return []conj{{path: e.args[0], pathSet: true}}, nil
	case "PathPrefix":
		var out []conj
		for _, a := range e.args {
			out = append(out, conj{path: a, prefix: true, pathSet: true})
		}
		return out, nil
	default:
		return nil, fmt.Errorf("rule: matcher %s() is not supported (supported: Host, Path, PathPrefix)", e.name)
	}
}

// ruleParser is a recursive-descent parser over the token stream.
// Grammar: or := and ("||" and)* ; and := term ("&&" term)* ;
// term := "(" or ")" | ident "(" string ("," string)* ")".
type ruleParser struct {
	toks []ruleToken
	pos  int
}

func (p *ruleParser) peek() (ruleToken, bool) {
	if p.pos >= len(p.toks) {
		return ruleToken{}, false
	}
	return p.toks[p.pos], true
}

func (p *ruleParser) parseOr() (ruleExpr, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for {
		t, ok := p.peek()
		if !ok || t.kind != tokOr {
			return left, nil
		}
		p.pos++
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = orExpr{left, right}
	}
}

func (p *ruleParser) parseAnd() (ruleExpr, error) {
	left, err := p.parseTerm()
	if err != nil {
		return nil, err
	}
	for {
		t, ok := p.peek()
		if !ok || t.kind != tokAnd {
			return left, nil
		}
		p.pos++
		right, err := p.parseTerm()
		if err != nil {
			return nil, err
		}
		left = andExpr{left, right}
	}
}

func (p *ruleParser) parseTerm() (ruleExpr, error) {
	t, ok := p.peek()
	if !ok {
		return nil, fmt.Errorf("rule: unexpected end of rule")
	}
	if t.kind == tokLParen {
		p.pos++
		inner, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if err := p.expect(tokRParen, ")"); err != nil {
			return nil, err
		}
		return inner, nil
	}
	if t.kind != tokIdent {
		return nil, fmt.Errorf("rule: expected matcher, got %q", t.val)
	}
	p.pos++
	if err := p.expect(tokLParen, "("); err != nil {
		return nil, err
	}
	args, err := p.parseArgs(t.val)
	if err != nil {
		return nil, err
	}
	return fnExpr{name: t.val, args: args}, nil
}

// parseArgs consumes a matcher's argument list up to and including the
// closing parenthesis.
func (p *ruleParser) parseArgs(fn string) ([]string, error) {
	var args []string
	for {
		at, ok := p.peek()
		if !ok {
			return nil, fmt.Errorf("rule: unterminated %s(", fn)
		}
		switch at.kind {
		case tokString:
			args = append(args, at.val)
			p.pos++
			if ct, ok := p.peek(); ok && ct.kind == tokComma {
				p.pos++
			}
		case tokRParen:
			p.pos++
			return args, nil
		default:
			return nil, fmt.Errorf("rule: unexpected %q in %s(...)", at.val, fn)
		}
	}
}

func (p *ruleParser) expect(kind int, what string) error {
	t, ok := p.peek()
	if !ok || t.kind != kind {
		got := "end of rule"
		if ok {
			got = fmt.Sprintf("%q", t.val)
		}
		return fmt.Errorf("rule: expected %q, got %s", what, got)
	}
	p.pos++
	return nil
}
