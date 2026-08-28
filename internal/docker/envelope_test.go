package docker

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func global() []Matcher { return []Matcher{m("", "/*")} }

// hostsRule builds Host(`h0`, … `hn-1`).
func hostsRule(n int) string {
	args := make([]string, 0, n)
	for i := range n {
		args = append(args, fmt.Sprintf("`h%d.example.com`", i))
	}
	return "Host(" + strings.Join(args, ", ") + ")"
}

// prefixArgs builds `/p0`, … `/pn-1`.
func prefixArgs(n int) string {
	args := make([]string, 0, n)
	for i := range n {
		args = append(args, fmt.Sprintf("`/p%d`", i))
	}
	return strings.Join(args, ", ")
}

// A rule ParseRule accepts and a container that can serve it leave routes
// and no refusal. This drives Extract, because a tombstone beside a healthy
// route would refuse traffic the router is serving.
func TestRuleEnvelopeAcceptedRules(t *testing.T) {
	t.Parallel()
	accepted := []string{
		"Host(`app.example.com`)",
		"Path(`/login`)",
		"PathPrefix(`/api`)",
		"PathPrefix(`/`)",
		"Host(`app.example.com`) && PathPrefix(`/api`)",
		"Host(`a.example.com`) || Host(`b.example.com`)",
		"Host(`a.example.com`, `b.example.com`) && PathPrefix(`/api`)",
		"Path(`/foo/bar`)",
	}
	for _, rule := range accepted {
		t.Run(rule, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseRule(rule); err != nil {
				t.Fatalf("ParseRule(%q) = %v, want it to be accepted", rule, err)
			}
			c := webContainer(map[string]string{
				"traefik.enable":                                     "true",
				"traefik.http.routers.app.rule":                      rule,
				"traefik.http.services.app.loadbalancer.server.port": "8080",
			})
			svcs, tombs, _ := Extract(c, ExtractOptions{TraefikLabels: true})
			if len(svcs) != 1 {
				t.Fatalf("Extract(%q) produced %d services, want the router to be served", rule, len(svcs))
			}
			if len(tombs) != 0 {
				t.Errorf("Extract(%q) produced routes and the refusal %v; a served router must leave no tombstone", rule, tombs)
			}
		})
	}
}

// A router with no rule declares no match condition, so no envelope is needed.
func TestRuleEnvelopeBlankRule(t *testing.T) {
	t.Parallel()
	for _, rule := range []string{"", "   ", "\t\n"} {
		if got := RuleEnvelope(rule); len(got) != 0 {
			t.Errorf("RuleEnvelope(%q) = %v, want no tombstone", rule, got)
		}
	}
}

func TestRuleEnvelope(t *testing.T) {
	t.Parallel()
	cases := []struct {
		rule string
		want []Matcher
		why  string
	}{
		{
			rule: "Host(`admin.example.com`) && ClientIP(`10.0.0.0/8`)",
			want: []Matcher{m("admin.example.com", "/*")},
			why:  "ClientIP contributes nothing, so the Host conjunct survives; the CIDR is never rebuilt onto the tombstone",
		}, {
			rule: "Host(`admin.example.com`) && PathPrefix(`/api`) && Header(`X-Token`, `t`)",
			want: []Matcher{px("admin.example.com", "/api")},
			why:  "both representable conjuncts survive; the header was the whole authorization story",
		}, {
			rule: "PathPrefix(`/private`) && ClientIP(`10.0.0.0/8`)",
			want: []Matcher{px("", "/private")},
			why:  "no Host conjunct exists, so the envelope must stay host-less",
		}, {
			rule: "Path(`/login`) && Method(`POST`)",
			want: []Matcher{m("", "/login")},
			why:  "an exact-path envelope is legitimate; it refuses GET /login too, which is allowed",
		}, {
			rule: "ClientIP(`10.0.0.0/8`)",
			want: global(),
			why:  "the rule constrains neither host nor path, so only the global envelope is a superset",
		}, {
			rule: "HostRegexp(`^admin\\..*\\.example\\.com$`)",
			want: global(),
			why:  "statute has no wildcard-host matcher and alternation defeats suffix mining in principle",
		}, {
			rule: "PathRegexp(`^/api/v[0-9]+/admin`)",
			want: global(),
			why:  "proving a regexp is contained in a prefix needs anchoring and alternation analysis",
		}, {
			rule: "HostSNI(`admin.example.com`)",
			want: global(),
			why:  "HostSNI names the TLS handshake, not the HTTP Host the route matcher inspects",
		}, {
			rule: "HostRegexp(`^a.*`) && PathPrefix(`/api`)",
			want: []Matcher{px("", "/api")},
			why:  "an unconstrained host and a real path coexist in one envelope",
		}, {
			rule: "Host(`a.example.com`) && FooBar(`x`)",
			want: []Matcher{m("a.example.com", "/*")},
			why:  "an unsupported matcher name leaves the sibling conjunct fully recoverable",
		}, {
			rule: "Host(`APP.Example.COM`) && Query(`z=1`)",
			want: []Matcher{m("app.example.com", "/*")},
			why:  "the envelope lowercases exactly as the accepted route would have",
		}, {
			rule: "Host(``) && Header(`X-K`, `v`)",
			want: global(),
			why:  "an empty host argument constrains nothing, which is a widening and hence sound",
		}, {
			rule: "Host(`a.example.com`) || ClientIP(`10.0.0.0/8`)",
			want: global(),
			why:  "branch-aware union: keeping only the understood branch is the headline subset bug",
		}, {
			rule: "(Host(`a.example.com`) && PathPrefix(`/x`)) || (Host(`b.example.com`) && Query(`z=1`))",
			want: []Matcher{px("a.example.com", "/x"), m("b.example.com", "/*")},
			why:  "one branch is exact and the other widens only its own path",
		}, {
			rule: "Host(`a.example.com`) && Header(`X`, `y`) || Host(`b.example.com`) && ClientIP(`10.0.0.0/8`)",
			want: []Matcher{m("a.example.com", "/*"), m("b.example.com", "/*")},
			why:  "'&&' binds tighter than '||', so neither branch is unconstrained",
		}, {
			rule: "(Host(`a.example.com`) || PathPrefix(`/x`)) && ClientIP(`10.0.0.0/8`)",
			want: []Matcher{px("", "/x"), m("a.example.com", "/*")},
			why:  "the disjuncts constrain different dimensions, so neither subsumes the other",
		}, {
			rule: "(Host(`a.example.com`) || Header(`X-K`, `v`)) && PathPrefix(`/x`)",
			want: []Matcher{px("", "/x")},
			why:  "the inner union unconstrains the host, then the outer meet re-narrows the path",
		}, {
			rule: "Host(`a.example.com`) && (PathPrefix(`/x`) || ClientIP(`10.0.0.0/8`))",
			want: []Matcher{m("a.example.com", "/*")},
			why:  "the second branch subsumes the first; stopping at the first branch would emit /x/*",
		}, {
			rule: "Host(`a.example.com`, `b.example.com`) && Query(`x=1`)",
			want: []Matcher{m("a.example.com", "/*"), m("b.example.com", "/*")},
			why:  "a multi-argument Host is a disjunction, so the host set renders one element per literal",
		}, {
			rule: "!Host(`a.example.com`)",
			want: global(),
			why:  "the complement of one host is every other host on every path",
		}, {
			rule: "Host(`app.example.com`) && !PathPrefix(`/public`)",
			want: []Matcher{m("app.example.com", "/*")},
			why:  "the negation node becomes unconstrained in place; the sibling host is not poisoned",
		}, {
			rule: "!(Host(`a.example.com`) && PathPrefix(`/x`))",
			want: global(),
			why:  "a negated whole disjunct is always global",
		}, {
			rule: "PathPrefix(`/a`) && PathPrefix(`/a/b`)",
			want: []Matcher{px("", "/a")},
			why:  "two path constraints keep the enclosing operand, never the narrower one",
		}, {
			rule: "Host(`x.example.com`) && PathPrefix(`/api`) && Path(`/api/health`)",
			want: []Matcher{px("x.example.com", "/api")},
			why:  "the exact path is inside the prefix, so the prefix is the wider sound choice",
		}, {
			rule: "Path(`/a`) && Path(`/b`)",
			want: []Matcher{m("", "/a")},
			why:  "incomparable operands take the leftmost; an emptiness argument would not generalize",
		}, {
			rule: "Host(`a.example.com`) && Host(`b.example.com`)",
			want: []Matcher{m("a.example.com", "/*")},
			why:  "'disjoint hosts can never match' is not a proof of an empty request set",
		}, {
			rule: "Host(`A.example.com`) && Host(`a.example.com`)",
			want: []Matcher{m("a.example.com", "/*")},
			why:  "the parser compares hosts raw while the dispatcher folds case: this rule is rejected yet matches",
		}, {
			rule: "Host(`a.example.com`) && Host(`b.example.com`) || Host(`c.example.com`) && ClientIP(`10.0.0.0/8`)",
			want: []Matcher{m("a.example.com", "/*"), m("c.example.com", "/*")},
			why:  "the deriver must stay branch-aware on the error path too",
		}, {
			rule: "Host(`a.example.com`) &&",
			want: global(),
			why:  "a parse failure is global: '&&' may be a typo for '||', so the parsed prefix is a candidate subset",
		}, {
			rule: "Host(`a.example",
			want: global(),
			why:  "an unterminated string loses every token, and the remainder may have carried a '||'",
		}, {
			rule: "Host(`a` && Path(`/x`)",
			want: global(),
			why:  "all four backticks pair, so this lexes cleanly and fails in the argument list instead",
		}, {
			rule: "Host(`a.example.com`) Host(`b.example.com`)",
			want: global(),
			why:  "trailing tokens: both operands parsed but the connective is unknown",
		}, {
			rule: "Host(`a.example.com`) & Host(`b.example.com`)",
			want: global(),
			why:  "a single '&' has no defensible reading, so widen rather than guess",
		}, {
			rule: "Host(app.example.com)",
			want: global(),
			why:  "the visible identifier is suggestive, not authoritative, and a narrow guess is the bug",
		}, {
			rule: "Host(`a.example.com`) && Foo_Bar(`x`)",
			want: global(),
			why:  "'_' is not an identifier character, so this fails at the lex stage and loses the Host tokens",
		}, {
			rule: "Host()",
			want: global(),
			why:  "a zero-argument matcher bounds nothing and, being the whole rule, gives the global envelope",
		}, {
			rule: "Host(`a.example.com`) && Path()",
			want: []Matcher{m("a.example.com", "/*")},
			why:  "a conjunction is contained in each operand, so an operand that bounds nothing is dropped like any other unrepresentable conjunct",
		}, {
			rule: "Path() && Host(`a.example.com`)",
			want: []Matcher{m("a.example.com", "/*")},
			why:  "the meet is symmetric; reading only the right operand's flag would widen this one back to global",
		}, {
			rule: "Host(`a.example.com`) && Path() && PathPrefix(`/api`)",
			want: []Matcher{px("a.example.com", "/api")},
			why:  "the unbounded conjunct leaves the other two to meet as they always would",
		}, {
			rule: "Host() && Path()",
			want: global(),
			why:  "neither operand bounds anything, so the meet has nothing to keep and stays the top envelope",
		}, {
			rule: "(Host(`a.example.com`) && Path()) || Host(`b.example.com`)",
			want: []Matcher{m("a.example.com", "/*"), m("b.example.com", "/*")},
			why:  "a branch narrowed past its unbounded conjunct reaches the union as a bounded branch, so the union stays branch-scoped",
		}, {
			rule: "Path() || Host(`a.example.com`)",
			want: global(),
			why:  "a globally widened branch must poison the union; keeping only the readable branch is the subset bug",
		}, {
			rule: "Host(`api.example.com`) && Path(`/users/{id:[0-9]+}`) && ClientIP(`10.0.0.0/8`)",
			want: []Matcher{m("api.example.com", "/*")},
			why:  "a placeholder path is not read literally, so only the host survives",
		}, {
			rule: "PathPrefix(`/foo/{id:[0-9]+}`) && ClientIP(`10.0.0.0/8`)",
			want: global(),
			why:  "truncating a placeholder to its segment boundary needs a grammar this repository cannot pin",
		}, {
			rule: "Host(`{sub:[a-z]+}.example.com`) && ClientIP(`10.0.0.0/8`)",
			want: global(),
			why:  "mining `example.com` would miss every sub-domain the placeholder really covered",
		}, {
			rule: "Path(`/a%20b`) && ClientIP(`10.0.0.0/8`)",
			want: global(),
			why:  "req.URL.Path is percent-decoded, so the literal under-refuses in both directions",
		}, {
			rule: "Host(`*`)",
			want: global(),
			why:  "Host star is Traefik's any-host matcher; a literal star would under-refuse",
		}, {
			rule: "Host(`*.example.com`)",
			want: global(),
			why:  "statute has no wildcard-host matcher, so the envelope cannot mine a suffix",
		}, {
			rule: "Host(`*`) && PathPrefix(`/api`)",
			want: []Matcher{px("", "/api")},
			why:  "the unreadable host drops, the PathPrefix conjunct still bounds the rule",
		}, {
			rule: hostsRule(65),
			want: global(),
			why:  "no proper subset of 65 hosts is a superset of their union, and splitting reintroduces route-table inflation",
		}, {
			rule: "Host(`a.example.com`) && PathPrefix(" + prefixArgs(65) + ")",
			want: []Matcher{m("a.example.com", "/*")},
			why:  "the ladder joins paths first, so a host-scoped envelope survives the cap",
		},
	}
	for _, tc := range cases {
		t.Run(tc.rule, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseRule(tc.rule); err == nil {
				t.Fatalf("ParseRule accepted %q; this table is for rules it rejects", tc.rule)
			}
			got := RuleEnvelope(tc.rule)
			if !matchersEqual(got, tc.want) {
				t.Errorf("RuleEnvelope(%q):\n got %v\nwant %v\nwhy: %s", tc.rule, got, tc.want, tc.why)
			}
		})
	}
}

// 9 hosts crossed with 8 prefixes trip the matcher cap. Collapsing paths
// keeps the fallback alive for every other host on the listener.
func TestRuleEnvelopeCapLadderKeepsHosts(t *testing.T) {
	t.Parallel()
	branches := make([]string, 0, 9)
	for i := range 9 {
		branches = append(branches, fmt.Sprintf("Host(`h%d.example.com`)", i))
	}
	rule := "(" + strings.Join(branches, " || ") + ") && PathPrefix(" + prefixArgs(8) + ")"
	if _, err := ParseRule(rule); err == nil {
		t.Fatal("ParseRule accepted the oversized rule")
	}
	want := make([]Matcher, 0, 9)
	for i := range 9 {
		want = append(want, m(fmt.Sprintf("h%d.example.com", i), "/*"))
	}
	if got := RuleEnvelope(rule); !matchersEqual(got, want) {
		t.Errorf("RuleEnvelope:\n got %v\nwant %v", got, want)
	}
}

// Matchers that parsed successfully still widen where the dispatcher cannot
// compare the literal faithfully.
func TestEnvelopeOfNormalizesLiterals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []Matcher
		want []Matcher
	}{
		{"canonical Traefik host is unchanged", []Matcher{m("a.example.com", "/*")}, []Matcher{m("a.example.com", "/*")}},
		{"non-ASCII host", []Matcher{m("café.example.com", "/x")}, []Matcher{m("", "/x")}},
		{"percent escape", []Matcher{m("a.example.com", "/a%20b")}, []Matcher{m("a.example.com", "/*")}},
		{"absorption", []Matcher{m("a.example.com", "/x/*"), m("", "/x/*")}, []Matcher{m("", "/x/*")}},
		{"global absorbs", []Matcher{m("a.example.com", "/x"), m("", "/*")}, global()},
		{"middleware stripped", []Matcher{{Host: "a", HostKind: HostExact, Path: "/*", PathKind: PathAny, Middlewares: []string{"auth"}}}, []Matcher{native("a", "/*")}},
		{"native host spelling preserved", []Matcher{native("a.example.com.", "/*")}, []Matcher{native("a.example.com.", "/*")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := EnvelopeOf(tc.in); !matchersEqual(got, tc.want) {
				t.Errorf("EnvelopeOf(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestEnvelopeOfMixedPathSemantics(t *testing.T) {
	t.Parallel()
	segment := Matcher{Path: "/api/*", PathKind: PathSegment}
	bytePrefix := Matcher{Path: "/api", PathKind: PathByte}
	bytePrefixSlash := Matcher{Path: "/api/", PathKind: PathByte}

	tests := []struct {
		name string
		in   []Matcher
		want []Matcher
	}{
		{"byte prefix covers equal-base segment prefix", []Matcher{segment, bytePrefix}, []Matcher{bytePrefix}},
		{"absorption is order independent", []Matcher{bytePrefix, segment}, []Matcher{bytePrefix}},
		{"segment prefix covers byte prefix below boundary", []Matcher{segment, bytePrefixSlash}, []Matcher{segment}},
		{"native exact host is absorbed by Traefik host", []Matcher{native("a.example.com", "/*"), m("a.example.com", "/*")}, []Matcher{m("a.example.com", "/*")}},
		{"native extra-dot host is absorbed by Traefik undotted host", []Matcher{native("a.example.com.", "/*"), m("a.example.com", "/*")}, []Matcher{m("a.example.com", "/*")}},
		{"native double-dot host is not absorbed by Traefik undotted host", []Matcher{native("a.example.com..", "/*"), m("a.example.com", "/*")}, []Matcher{m("a.example.com", "/*"), native("a.example.com..", "/*")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := EnvelopeOf(tc.in); !matchersEqual(got, tc.want) {
				t.Fatalf("EnvelopeOf(%+v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestRuleEnvelopeKeepsDistinctSingleDotHosts(t *testing.T) {
	t.Parallel()
	// Host("0.") matches 0, 0., and 0..; Host("0") is a subset and absorbs.
	rule := "Host(`0.`, `0`)"
	want := []Matcher{m("0.", "/*")}
	if got := RuleEnvelope(rule); !matchersEqual(got, want) {
		t.Fatalf("RuleEnvelope(%q) = %+v, want %+v", rule, got, want)
	}
	routes, err := ParseRule(rule)
	if err != nil {
		t.Fatalf("ParseRule(%q): %v", rule, err)
	}
	env := RuleEnvelope(rule)
	for _, mt := range routes {
		for _, req := range probeRequests(mt) {
			host, path := req[0], req[1]
			if matchesRequest(mt, host, path) && !matchesAny(env, host, path) {
				t.Fatalf("request %q %q matches %+v but misses the envelope", host, path, mt)
			}
		}
	}
}

func TestRuleEnvelopeKeepsExactDoubleDotAndZeroRegression(t *testing.T) {
	t.Parallel()
	rule := "Host(`0..`, `0`)"
	want := []Matcher{
		m("0", "/*"),
		m("0..", "/*"),
	}
	if got := RuleEnvelope(rule); !matchersEqual(got, want) {
		t.Fatalf("RuleEnvelope(%q) = %+v, want %+v", rule, got, want)
	}
}

func matchersEqual(a, b []Matcher) bool {
	return slices.EqualFunc(a, b, Matcher.Equal)
}
