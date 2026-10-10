package statutelifecycle

import (
	"go/ast"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/analysistest"
)

func TestAnalyzer(t *testing.T) {
	t.Parallel()
	// Formatters simplify redundant source parentheses. Preserve that AST
	// regression after type checking without changing the production Analyzer.
	analyzer := *Analyzer
	parenthesized := false
	analyzer.Run = func(pass *analysis.Pass) (any, error) {
		for _, file := range pass.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Name.Name != "selectedParenthesesStart" {
					continue
				}
				ast.Inspect(fn.Body, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok {
						return true
					}
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
						call.Fun = &ast.ParenExpr{Lparen: sel.Pos(), X: sel, Rparen: sel.End() - 1}
						parenthesized = true
					}
					return true
				})
			}
		}
		return Analyzer.Run(pass)
	}
	analysistest.Run(t, filepath.Join(testdataDir(t), "core"), &analyzer, "a")
	if !parenthesized {
		t.Fatal("parenthesized deferred selector fixture was not exercised")
	}
}

func TestDockerMutationAnalyzer(t *testing.T) {
	t.Parallel()
	for _, fixture := range []string{
		"valid",
		"mutation-state",
		"uncertainty-rejection",
		"uncertainty-unrecorded",
		"phase-guard",
		"state-refactor",
		"observation-state",
		"retention-state",
		"retention-prune-invalid",
		"retention-prune-break",
		"supersession-refactor",
		"supersession-goto",
		"raw-call",
		"boundary-context",
		"missing-cancellation",
		"asynchronous-call",
		"persistence",
		"settlement-boundaries",
		"settlement-delete-order",
		"settlement-registry-provenance",
		"settlement-owner-revalidation",
		"settlement-ownership-order",
		"settlement-generation-fencing",
	} {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()
			analysistest.Run(t, filepath.Join(testdataDir(t), "docker", fixture), Analyzer, "statute.kjanat.dev")
		})
	}
}

func testdataDir(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate analyzer testdata")
	}
	return filepath.Join(filepath.Dir(filename), "testdata")
}
