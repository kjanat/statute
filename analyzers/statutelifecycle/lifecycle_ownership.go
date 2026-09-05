package statutelifecycle

import (
	"go/ast"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
)

func collectLifecycleStarts(functions map[*types.Func]*functionInfo) map[*types.Func]*lifecycleStart {
	out := make(map[*types.Func]*lifecycleStart)
	for _, info := range functions {
		sig, _ := info.fn.Type().(*types.Signature)
		if sig == nil || sig.Recv() == nil || !isStartMethodName(info.fn.Name()) {
			continue
		}
		relation := &lifecycleStart{start: info, ownerResults: make(map[int]bool)}
		for i := range sig.Results().Len() {
			methods := cleanupMethods(sig.Results().At(i).Type(), functions)
			if len(methods) == 0 {
				continue
			}
			relation.ownerResults[i] = true
			relation.owners = append(relation.owners, lifecycleOwner{cleanups: methods, result: i})
		}
		if len(relation.owners) == 0 {
			methods := cleanupMethods(sig.Recv().Type(), functions)
			if len(methods) > 0 {
				relation.owners = append(relation.owners, lifecycleOwner{cleanups: methods, result: -1})
			}
		}
		if len(relation.owners) > 0 {
			out[info.fn] = relation
		}
	}
	return out
}

func cleanupMethods(owner types.Type, functions map[*types.Func]*functionInfo) []cleanupMethod {
	sets := []*types.MethodSet{types.NewMethodSet(owner)}
	if _, pointer := types.Unalias(owner).(*types.Pointer); !pointer {
		if named := namedType(owner); named != nil {
			sets = append(sets, types.NewMethodSet(types.NewPointer(named)))
		}
	}
	var out []cleanupMethod
	for _, set := range sets {
		for method := range set.Methods() {
			fn, _ := method.Obj().(*types.Func)
			if fn == nil || !isCleanupMethodName(fn.Name()) {
				continue
			}
			out = append(out, cleanupMethod{fn: fn, info: functions[fn]})
		}
	}
	return uniqueCleanupMethods(out)
}

func uniqueCleanupMethods(methods []cleanupMethod) []cleanupMethod {
	seen := make(map[*types.Func]bool)
	out := make([]cleanupMethod, 0, len(methods))
	for _, method := range methods {
		if seen[method.fn] {
			continue
		}
		seen[method.fn] = true
		out = append(out, method)
	}
	return out
}

func checkLifecycleStartCalls(pass *analysis.Pass, info *functionInfo, lifecycle map[*types.Func]*lifecycleStart, parents map[ast.Node]ast.Node) {
	constructor := strings.HasPrefix(info.fn.Name(), "new") && len(info.fn.Name()) > len("new")
	ast.Inspect(info.decl.Body, func(node ast.Node) bool {
		if node == nil {
			return false
		}
		if _, ok := node.(*ast.FuncLit); ok {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn := calledFunction(pass, call)
		relation := lifecycle[fn]
		if relation == nil {
			return true
		}
		if constructor {
			pass.Reportf(call.Pos(),
				"["+diagnosticSLC101+"] constructor %s starts lifecycle-owned state; construct it here and start it from the owning Start phase so rollback can stop it",
				info.fn.Name())
			return true
		}
		if len(relation.ownerResults) > 0 && discardedLifecycleOwner(call, relation.ownerResults, parents) {
			pass.Reportf(call.Pos(),
				"["+diagnosticSLC101+"] discarded lifecycle owner returned by %s; retain it so cleanup can stop the started generation",
				fn.Name())
		}
		return true
	})
}

func discardedLifecycleOwner(call *ast.CallExpr, ownerResults map[int]bool, parents map[ast.Node]ast.Node) bool {
	parent := enclosingExpressionParent(call, parents)
	switch p := parent.(type) {
	case *ast.ExprStmt, *ast.GoStmt, *ast.DeferStmt:
		return true
	case *ast.AssignStmt:
		return assignmentDiscardsOwner(call, p, ownerResults)
	default:
		return false
	}
}

func enclosingExpressionParent(expr ast.Expr, parents map[ast.Node]ast.Node) ast.Node {
	parent := parents[expr]
	for {
		paren, ok := parent.(*ast.ParenExpr)
		if !ok {
			return parent
		}
		parent = parents[paren]
	}
}

func assignmentDiscardsOwner(call *ast.CallExpr, assign *ast.AssignStmt, ownerResults map[int]bool) bool {
	if len(assign.Rhs) == 1 && assign.Rhs[0] == call {
		for result := range ownerResults {
			if result >= len(assign.Lhs) || isBlankIdent(assign.Lhs[result]) {
				return true
			}
		}
		return false
	}
	for i, rhs := range assign.Rhs {
		if rhs == call {
			return ownerResults[0] && (i >= len(assign.Lhs) || isBlankIdent(assign.Lhs[i]))
		}
	}
	return false
}

func isBlankIdent(expr ast.Expr) bool {
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == "_"
}

func isStartMethodName(name string) bool {
	return name == methodStart || name == methodStartAPI
}

func isCleanupMethodName(name string) bool {
	switch name {
	case methodStop, methodStopRun, methodShutdownLocal, methodShutdown, methodClose:
		return true
	default:
		return false
	}
}
