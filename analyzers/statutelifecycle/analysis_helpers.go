package statutelifecycle

import (
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
)

func assumeCallReturns(*ast.CallExpr) bool { return true }

func isSyncMethodCall(pass *analysis.Pass, call *ast.CallExpr, owner, method string) bool {
	fn := calledFunction(pass, call)
	if fn == nil || fn.Name() != method {
		return false
	}
	sig, _ := fn.Type().(*types.Signature)
	if sig == nil || sig.Recv() == nil {
		return false
	}
	named := namedType(sig.Recv().Type())
	return named != nil && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "sync" && named.Obj().Name() == owner
}

func parentMap(files []*ast.File) map[ast.Node]ast.Node {
	parents := make(map[ast.Node]ast.Node)
	for _, file := range files {
		var stack []ast.Node
		ast.Inspect(file, func(node ast.Node) bool {
			if node == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			if len(stack) > 0 {
				parents[node] = stack[len(stack)-1]
			}
			stack = append(stack, node)
			return true
		})
	}
	return parents
}

func calledFunction(pass *analysis.Pass, call *ast.CallExpr) *types.Func {
	var obj types.Object
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		obj = pass.TypesInfo.Uses[fun]
	case *ast.SelectorExpr:
		obj = pass.TypesInfo.Uses[fun.Sel]
	}
	fn, _ := obj.(*types.Func)
	return fn
}

func namedType(t types.Type) *types.Named {
	for {
		t = types.Unalias(t)
		pointer, ok := t.(*types.Pointer)
		if !ok {
			break
		}
		t = pointer.Elem()
	}
	named, _ := t.(*types.Named)
	return named
}

func functionReturnsError(fn *types.Func) bool {
	sig, _ := fn.Type().(*types.Signature)
	return sig != nil && signatureReturnsError(sig)
}

func signatureReturnsError(sig *types.Signature) bool {
	errType := types.Universe.Lookup("error").Type()
	for result := range sig.Results().Variables() {
		if types.Identical(result.Type(), errType) {
			return true
		}
	}
	return false
}

// aliasTarget is what a single-assignment local alias resolves to.
type aliasTarget struct {
	root *types.Var
	path string
}

// pathResolver normalizes expressions within one function body. A variable
// the body reassigns after its definition, or takes the address of, is
// never resolvable. Field writes and escapes invalidate every path below
// the written prefix. Pointer aliases preserve storage identity.
type pathResolver struct {
	pass        *analysis.Pass
	aliases     map[*types.Var]aliasTarget
	aliasRHS    map[ast.Expr]bool
	addrAliases map[*types.Var]bool
	written     map[*types.Var][]string
	mutated     map[*types.Var]bool
}

func newPathResolver(pass *analysis.Pass, body *ast.BlockStmt) *pathResolver {
	r := &pathResolver{
		pass:        pass,
		aliases:     make(map[*types.Var]aliasTarget),
		aliasRHS:    make(map[ast.Expr]bool),
		addrAliases: make(map[*types.Var]bool),
		written:     make(map[*types.Var][]string),
		mutated:     collectMutatedVars(pass, body),
	}
	r.collectAliases(body)
	r.collectWrittenPaths(body)
	r.collectAliasEscapes(body)
	return r
}

// resolve normalizes expr to a root variable plus the complete field path,
// refusing paths whose storage may have been replaced or escaped.
func (r *pathResolver) resolve(expr ast.Expr) (*types.Var, string, bool) {
	root, path, ok := r.resolveExpr(expr)
	if !ok || r.pathInvalidated(root, path) {
		return nil, "", false
	}
	return root, path, true
}

func (r *pathResolver) pathInvalidated(root *types.Var, path string) bool {
	for _, written := range r.written[root] {
		if written == path || strings.HasPrefix(path, written+".") {
			return true
		}
	}
	return false
}

func (r *pathResolver) resolveExpr(expr ast.Expr) (*types.Var, string, bool) {
	switch e := ast.Unparen(expr).(type) {
	case *ast.Ident:
		v, _ := r.pass.TypesInfo.Uses[e].(*types.Var)
		if v == nil || r.mutated[v] {
			return nil, "", false
		}
		if target, ok := r.aliases[v]; ok {
			return target.root, target.path, true
		}
		return v, "", true
	case *ast.SelectorExpr:
		return r.resolveSelector(e)
	case *ast.UnaryExpr:
		if e.Op == token.AND {
			return r.resolveExpr(e.X)
		}
	case *ast.StarExpr:
		return r.resolveExpr(e.X)
	}
	return nil, "", false
}

func (r *pathResolver) resolveSelector(e *ast.SelectorExpr) (*types.Var, string, bool) {
	sel := r.pass.TypesInfo.Selections[e]
	if sel == nil || sel.Kind() != types.FieldVal {
		return nil, "", false
	}
	root, base, ok := r.resolveExpr(e.X)
	if !ok {
		return nil, "", false
	}
	path, ok := selectionFieldPath(sel)
	if !ok {
		return nil, "", false
	}
	return root, base + path, true
}

func selectionFieldPath(sel *types.Selection) (string, bool) {
	var b strings.Builder
	current := sel.Recv()
	for _, index := range sel.Index() {
		st := underlyingStruct(current)
		if st == nil || index >= st.NumFields() {
			return "", false
		}
		field := st.Field(index)
		b.WriteString(".")
		b.WriteString(field.Name())
		current = field.Type()
	}
	return b.String(), true
}

func underlyingStruct(t types.Type) *types.Struct {
	for {
		t = types.Unalias(t)
		if pointer, ok := t.(*types.Pointer); ok {
			t = pointer.Elem()
			continue
		}
		break
	}
	st, _ := t.Underlying().(*types.Struct)
	return st
}

func (r *pathResolver) collectAliases(body *ast.BlockStmt) {
	candidates := r.aliasCandidates(body)
	for changed := true; changed; {
		changed = false
		for v, rhs := range candidates {
			if _, done := r.aliases[v]; done {
				continue
			}
			root, path, ok := r.resolve(rhs)
			if !ok || root == v {
				continue
			}
			r.aliases[v] = aliasTarget{root: root, path: path}
			r.aliasRHS[rhs] = true
			if r.aliasTakesAddress(rhs) {
				r.addrAliases[v] = true
			}
			changed = true
		}
	}
}

func (r *pathResolver) aliasCandidates(body *ast.BlockStmt) map[*types.Var]ast.Expr {
	candidates := make(map[*types.Var]ast.Expr)
	ast.Inspect(body, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok || assign.Tok != token.DEFINE || len(assign.Lhs) != len(assign.Rhs) {
			return true
		}
		for i, lhs := range assign.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok {
				continue
			}
			v, _ := r.pass.TypesInfo.Defs[id].(*types.Var)
			if v == nil || r.mutated[v] || !isPointerType(r.pass.TypesInfo.TypeOf(assign.Rhs[i])) {
				continue
			}
			candidates[v] = assign.Rhs[i]
		}
		return true
	})
	return candidates
}

func (r *pathResolver) collectWrittenPaths(body *ast.BlockStmt) {
	ast.Inspect(body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.AssignStmt:
			for _, lhs := range n.Lhs {
				r.recordWrittenPath(lhs)
			}
		case *ast.IncDecStmt:
			r.recordWrittenPath(n.X)
		case *ast.RangeStmt:
			r.recordWrittenPath(n.Key)
			r.recordWrittenPath(n.Value)
		case *ast.UnaryExpr:
			if n.Op == token.AND && !r.aliasRHS[n] {
				r.recordWrittenPath(n.X)
			}
		}
		return true
	})
}

func (r *pathResolver) aliasTakesAddress(rhs ast.Expr) bool {
	switch e := ast.Unparen(rhs).(type) {
	case *ast.UnaryExpr:
		return e.Op == token.AND
	case *ast.Ident:
		v, _ := r.pass.TypesInfo.Uses[e].(*types.Var)
		return v != nil && r.addrAliases[v]
	}
	return false
}

func (r *pathResolver) collectAliasEscapes(body *ast.BlockStmt) {
	if len(r.addrAliases) == 0 {
		return
	}
	safe := safeAliasBaseUses(body)
	ast.Inspect(body, func(node ast.Node) bool {
		id, ok := node.(*ast.Ident)
		if !ok || safe[id] || r.aliasRHS[id] {
			return true
		}
		v, _ := r.pass.TypesInfo.Uses[id].(*types.Var)
		if v == nil || !r.addrAliases[v] {
			return true
		}
		target := r.aliases[v]
		r.written[target.root] = append(r.written[target.root], target.path)
		return true
	})
}

func safeAliasBaseUses(body *ast.BlockStmt) map[*ast.Ident]bool {
	safe := make(map[*ast.Ident]bool)
	mark := func(expr ast.Expr) {
		if id, ok := ast.Unparen(expr).(*ast.Ident); ok {
			safe[id] = true
		}
	}
	ast.Inspect(body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.SelectorExpr:
			mark(n.X)
		case *ast.StarExpr:
			mark(n.X)
		}
		return true
	})
	return safe
}

func (r *pathResolver) recordWrittenPath(expr ast.Expr) {
	if expr == nil {
		return
	}
	if _, ok := ast.Unparen(expr).(*ast.Ident); ok {
		return
	}
	if root, path, ok := r.resolveExpr(expr); ok {
		r.written[root] = append(r.written[root], path)
	}
}

func isPointerType(t types.Type) bool {
	if t == nil {
		return false
	}
	_, ok := types.Unalias(t).(*types.Pointer)
	return ok
}

func collectMutatedVars(pass *analysis.Pass, body *ast.BlockStmt) map[*types.Var]bool {
	mutated := make(map[*types.Var]bool)
	record := func(id *ast.Ident) {
		if v, _ := pass.TypesInfo.Uses[id].(*types.Var); v != nil {
			mutated[v] = true
		}
	}
	ast.Inspect(body, func(node ast.Node) bool {
		recordNodeWrites(node, record)
		return true
	})
	return mutated
}

func recordNodeWrites(node ast.Node, record func(*ast.Ident)) {
	recordIdent := func(expr ast.Expr) {
		if id, ok := expr.(*ast.Ident); ok {
			record(id)
		}
	}
	switch n := node.(type) {
	case *ast.AssignStmt:
		for _, lhs := range n.Lhs {
			recordIdent(lhs)
		}
	case *ast.IncDecStmt:
		recordIdent(n.X)
	case *ast.RangeStmt:
		recordIdent(n.Key)
		recordIdent(n.Value)
	case *ast.UnaryExpr:
		if n.Op == token.AND {
			recordIdent(ast.Unparen(n.X))
		}
	}
}
