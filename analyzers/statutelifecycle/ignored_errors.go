package statutelifecycle

import (
	"go/ast"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
)

func checkIgnoredLifecycleCalls(pass *analysis.Pass, info *functionInfo, functions map[*types.Func]*functionInfo, parents map[ast.Node]ast.Node) {
	lifecycleFn := isLifecycleFunction(info.fn.Name())
	ast.Inspect(info.decl.Body, func(node ast.Node) bool {
		if node == nil {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn := calledFunction(pass, call)
		if fn == nil || !callIgnored(call, parents) {
			return true
		}
		if isServeFunction(fn) || localPublisherReturnsError(fn, functions) {
			pass.Reportf(call.Pos(), "["+diagnosticSLC102+"] ignored %s error can leave a bound-but-dead serving endpoint; observe and retire unexpected Serve exits", fn.Name())
		}
		if lifecycleFn && isCleanupFunction(fn) && functionReturnsError(fn) {
			pass.Reportf(call.Pos(), "["+diagnosticSLC104+"] lifecycle cleanup discards %s error; propagate or join cleanup failures", fn.Name())
		}
		return true
	})
}

func localPublisherReturnsError(fn *types.Func, functions map[*types.Func]*functionInfo) bool {
	info := functions[fn]
	return info != nil && info.publishes && functionReturnsError(fn)
}

func isLifecycleFunction(name string) bool {
	lower := strings.ToLower(name)
	return isStartMethodName(name) || isCleanupMethodName(name) ||
		strings.HasPrefix(lower, "unwind") || strings.HasPrefix(lower, "rollback")
}

func isCleanupFunction(fn *types.Func) bool {
	return isCleanupMethodName(fn.Name())
}

//nolint:gocyclo // Go has several syntax forms that intentionally discard call results.
func callIgnored(call *ast.CallExpr, parents map[ast.Node]ast.Node) bool {
	parent := parents[call]
	switch p := parent.(type) {
	case *ast.ExprStmt:
		return p.X == call
	case *ast.GoStmt:
		return p.Call == call
	case *ast.DeferStmt:
		return p.Call == call
	case *ast.AssignStmt:
		if len(p.Rhs) != 1 || p.Rhs[0] != call {
			return false
		}
		for _, lhs := range p.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok || id.Name != "_" {
				return false
			}
		}
		return len(p.Lhs) > 0
	}
	return false
}
