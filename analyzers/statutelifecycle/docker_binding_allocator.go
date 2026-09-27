package statutelifecycle

import (
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/analysis"
)

const bindingCounterField = "nextWorkloadBinding"
const bindingCounterPath = ".nextWorkloadBinding"

func checkBindingAllocator(pass *analysis.Pass, info *functionInfo) {
	if !isLocalMethod(info.fn, "dockerProvider", "nextWorkloadBindingLocked") || !helperHasField(info.fn.Type().(*types.Signature).Recv().Type(), bindingCounterField) {
		return
	}
	sig := info.fn.Type().(*types.Signature)
	resolver := newPathResolver(pass, info.decl.Body)
	flow := newFunctionFlow(info.decl.Body)
	var increment *ast.IncDecStmt
	count := 0
	ast.Inspect(info.decl.Body, func(node ast.Node) bool {
		if change, ok := node.(*ast.IncDecStmt); ok && sameBindingStorage(resolver, change.X, sig.Recv(), bindingCounterPath) {
			increment = change
			count++
		}
		return true
	})
	ast.Inspect(info.decl.Body, func(node ast.Node) bool {
		ret, ok := node.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		if count != 1 || !bindingAllocatorReturn(pass, info, resolver, flow, sig, increment, ret) {
			pass.Reportf(ret.Pos(), "[%s] binding allocator must return its unique incremented provider counter", diagnosticSLC105)
		}
		return true
	})
}

func bindingAllocatorReturn(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, sig *types.Signature, increment *ast.IncDecStmt, ret *ast.ReturnStmt) bool {
	if increment == nil || increment.Tok != token.INC || len(ret.Results) != 1 {
		return false
	}
	result := stableDefinitionExpr(pass, info.decl.Body, ret.Results[0], 0)
	return sameBindingStorage(resolver, result, sig.Recv(), bindingCounterPath) && flow.dominates(increment, result) && flow.dominates(increment, ret) && !bindingCounterRepeated(increment, flow)
}

func bindingCounterRepeated(node ast.Node, flow *functionFlow) bool {
	point, ok := flow.point(node)
	if !ok {
		return true
	}
	for _, next := range point.block.Succs {
		if flow.blockReaches(next, point.block) {
			return true
		}
	}
	return false
}

func bindingCounterWrite(info *functionInfo, resolver *pathResolver, node ast.Node, lhs ast.Expr) bool {
	if !isLocalMethod(info.fn, "dockerProvider", "nextWorkloadBindingLocked") {
		return false
	}
	sig, _ := info.fn.Type().(*types.Signature)
	increment, ok := node.(*ast.IncDecStmt)
	return ok && increment.Tok == token.INC && sig != nil && sameBindingStorage(resolver, lhs, sig.Recv(), bindingCounterPath)
}
