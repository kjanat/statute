package statute

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The README lint rule table is the only reference index for the rule
// codes, so it is anchored and held to the rule set rather than trusted.
const (
	lintTableStart = "<!-- lint-rules:start -->"
	lintTableEnd   = "<!-- lint-rules:end -->"
)

// lintTableRow matches one documented rule: `CODE` then its severity.
var lintTableRow = regexp.MustCompile("^\\|\\s*`([A-Z]+[0-9]+)`\\s*\\|\\s*([a-z]+)\\s*\\|")

// severityIdents maps the identifiers lint.go writes into a Finding to the
// severity they name; validSeverities is the same set as the table spells it.
var (
	severityIdents = map[string]Severity{
		"SeverityWarning": SeverityWarning,
		"SeverityError":   SeverityError,
	}
	validSeverities = []Severity{SeverityWarning, SeverityError}
)

func TestLintRuleTableDocumentsEveryRule(t *testing.T) {
	t.Parallel()
	emitted := emittedRuleSeverities(t)
	documented := documentedRuleSeverities(t)
	for code, severities := range emitted {
		got, ok := documented[code]
		if !ok {
			t.Errorf("%s fires in lint.go but has no row in the README lint rule table", code)
			continue
		}
		if !slices.Contains(severities, got) {
			t.Errorf("%s is documented as %q; lint.go emits it as %v", code, got, severities)
		}
	}
	for code := range documented {
		if _, ok := emitted[code]; !ok {
			t.Errorf("the README lint rule table documents %s, which no rule in lint.go emits", code)
		}
	}
}

// emittedRuleSeverities collects every code lint.go can emit, with the
// severities it is emitted at. Scanning the source rather than running the
// rules covers codes no test config triggers.
func emittedRuleSeverities(t *testing.T) map[string][]Severity {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "lint.go", nil, 0)
	if err != nil {
		t.Fatalf("parse lint.go: %v", err)
	}
	out := make(map[string][]Severity)
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		code, severity, ok := findingFields(t, lit)
		if !ok {
			return true
		}
		if !slices.Contains(out[code], severity) {
			out[code] = append(out[code], severity)
		}
		return true
	})
	if len(out) == 0 {
		t.Fatal("no Finding literals found in lint.go")
	}
	return out
}

// findingFields reads the Code and Severity of a Finding literal. A literal
// carrying both is a Finding whether or not it names the type, which the
// inner elements of a []Finding literal do not.
func findingFields(t *testing.T, lit *ast.CompositeLit) (string, Severity, bool) {
	t.Helper()
	var code string
	var severity Severity
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		switch key.Name {
		case "Code":
			code = stringValue(t, kv.Value)
		case "Severity":
			ident, ok := kv.Value.(*ast.Ident)
			if !ok {
				t.Fatalf("lint.go: Finding severity is not an identifier (%T)", kv.Value)
			}
			severity, ok = severityIdents[ident.Name]
			if !ok {
				t.Fatalf("unknown severity identifier %q in lint.go", ident.Name)
			}
		}
	}
	return code, severity, code != "" && severity != ""
}

// stringValue unquotes a string literal, failing the test when the field
// holds anything a scan cannot read.
func stringValue(t *testing.T, expr ast.Expr) string {
	t.Helper()
	lit, ok := expr.(*ast.BasicLit)
	if !ok {
		t.Fatalf("lint.go: rule code is not a string literal (%T); the doc test cannot read it", expr)
	}
	value, err := strconv.Unquote(lit.Value)
	if err != nil {
		t.Fatalf("unquote %s: %v", lit.Value, err)
	}
	return value
}

// documentedRuleSeverities reads the anchored table out of README.md.
func documentedRuleSeverities(t *testing.T) map[string]Severity {
	t.Helper()
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	_, after, ok := strings.Cut(string(readme), lintTableStart)
	if !ok {
		t.Fatalf("README.md has no %s anchor", lintTableStart)
	}
	table, _, ok := strings.Cut(after, lintTableEnd)
	if !ok {
		t.Fatalf("README.md has no %s anchor", lintTableEnd)
	}
	out := make(map[string]Severity)
	for line := range strings.SplitSeq(table, "\n") {
		match := lintTableRow.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		severity := Severity(match[2])
		if !slices.Contains(validSeverities, severity) {
			t.Errorf("%s is documented with unknown severity %q", match[1], severity)
			continue
		}
		if previous, ok := out[match[1]]; ok {
			t.Errorf("%s has two rows in the README lint rule table (%q, %q)", match[1], previous, severity)
		}
		out[match[1]] = severity
	}
	return out
}
