package main

import (
	"fmt"
	"go/ast"
	"go/printer"
	"go/token"
	"strings"
)

// pkg.go.dev renders every symbol on a single line, eliding composite bodies:
// a forty-field struct is published as "struct{ ... }", an empty (or
// entirely unexported) one as "struct{}". The baseline we diff against only
// ever contains that form, so the local side has to be rendered the same way
// instead of being printed in full. The rules below mirror x/pkgsite's
// one-line renderer for the shapes that occur in Go type expressions;
// anything else falls through to go/printer, which is exact for the leaves
// (identifiers, selectors, calls, literals).

// oneLine renders an expression on a single line with composite bodies elided.
func oneLine(fset *token.FileSet, expr ast.Expr) string {
	if s, ok := oneLineBody(fset, expr); ok {
		return s
	}
	if s, ok := oneLineWrapper(fset, expr); ok {
		return s
	}
	return printNode(fset, expr)
}

// oneLineBody handles the node kinds whose bodies pkg.go.dev elides.
func oneLineBody(fset *token.FileSet, expr ast.Expr) (string, bool) {
	switch e := expr.(type) {
	case *ast.StructType:
		return "struct{" + elided(fieldCount(e.Fields)) + "}", true
	case *ast.InterfaceType:
		return "interface{" + elided(fieldCount(e.Methods)) + "}", true
	case *ast.CompositeLit:
		return oneLine(fset, e.Type) + "{" + elided(len(e.Elts)) + "}", true
	case *ast.FuncType:
		return "func" + signature(fset, e), true
	}
	return "", false
}

// oneLineWrapper handles the type constructors that wrap another expression.
// Each one recurses so an elided body stays elided at any nesting depth.
func oneLineWrapper(fset *token.FileSet, expr ast.Expr) (string, bool) {
	switch e := expr.(type) {
	case nil:
		return "", true
	case *ast.StarExpr:
		return "*" + oneLine(fset, e.X), true
	case *ast.UnaryExpr:
		return e.Op.String() + oneLine(fset, e.X), true
	case *ast.ParenExpr:
		return "(" + oneLine(fset, e.X) + ")", true
	case *ast.Ellipsis:
		return "..." + oneLine(fset, e.Elt), true
	case *ast.ArrayType:
		return "[" + oneLine(fset, e.Len) + "]" + oneLine(fset, e.Elt), true
	case *ast.MapType:
		return "map[" + oneLine(fset, e.Key) + "]" + oneLine(fset, e.Value), true
	case *ast.ChanType:
		return chanPrefix(e.Dir) + oneLine(fset, e.Value), true
	}
	return "", false
}

// signature renders a function type's type parameters, parameters and results:
// "[T any](a, b T) (int, error)", with parentheses around the results only
// where Go requires them.
func signature(fset *token.FileSet, ft *ast.FuncType) string {
	sig := ""
	if ft.TypeParams != nil {
		sig = "[" + strings.Join(fieldStrings(fset, ft.TypeParams), ", ") + "]"
	}
	sig += "(" + strings.Join(fieldStrings(fset, ft.Params), ", ") + ")"

	results := fieldStrings(fset, ft.Results)
	switch {
	case len(results) == 0:
		return sig
	case len(results) == 1 && len(ft.Results.List[0].Names) == 0:
		return sig + " " + results[0]
	default:
		return sig + " (" + strings.Join(results, ", ") + ")"
	}
}

// fieldStrings renders each entry of a parameter, result, receiver or type
// parameter list as "names type", or bare "type" where the entry is unnamed.
func fieldStrings(fset *token.FileSet, list *ast.FieldList) []string {
	if list == nil {
		return nil
	}
	out := make([]string, 0, len(list.List))
	for _, f := range list.List {
		typ := oneLine(fset, f.Type)
		if len(f.Names) == 0 {
			out = append(out, typ)
			continue
		}
		names := make([]string, 0, len(f.Names))
		for _, n := range f.Names {
			names = append(names, n.Name)
		}
		out = append(out, strings.Join(names, ", ")+" "+typ)
	}
	return out
}

// elided is the body pkg.go.dev prints for a composite with n members.
func elided(n int) string {
	if n == 0 {
		return ""
	}
	return " ... "
}

func fieldCount(list *ast.FieldList) int {
	if list == nil {
		return 0
	}
	return len(list.List)
}

func chanPrefix(dir ast.ChanDir) string {
	switch dir {
	case ast.RECV:
		return "<-chan "
	case ast.SEND:
		return "chan<- "
	default:
		return "chan "
	}
}

// printNode renders a node with go/printer and folds it onto one line.
func printNode(fset *token.FileSet, node any) string {
	var buf strings.Builder
	if err := printer.Fprint(&buf, fset, node); err != nil {
		return fmt.Sprintf("<unprintable: %v>", err)
	}
	return strings.Join(strings.Fields(buf.String()), " ")
}
