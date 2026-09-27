package statutelifecycle

import (
	"go/ast"
	"go/types"

	"golang.org/x/tools/go/analysis"
)

type bindingIdentityProof struct {
	pass        *analysis.Pass
	info        *functionInfo
	resolver    *pathResolver
	sig         *types.Signature
	binding     bool
	comparisons []ownerComparison
	scenario    int
	expected    bool
}

func checkBindingIdentityHelpers(pass *analysis.Pass, info *functionInfo) {
	checkBindingAllocator(pass, info)
	if isLocalMethod(info.fn, "workloadBinding", "sameContainer") {
		checkSameContainerHelper(pass, info, true)
	}
	if isLocalMethod(info.fn, "workload", "sameContainerLocked") {
		checkSameContainerHelper(pass, info, false)
	}
}

func checkSameContainerHelper(pass *analysis.Pass, info *functionInfo, binding bool) {
	sig, _ := info.fn.Type().(*types.Signature)
	if sig == nil || sig.Recv() == nil || sig.Params().Len() != 1 {
		return
	}
	field := mutationBindingField
	if binding {
		field = mutationContainerIDField
	}
	if !helperHasField(sig.Recv().Type(), field) {
		return
	}
	p := bindingIdentityProof{pass: pass, info: info, resolver: newPathResolver(pass, info.decl.Body), sig: sig, binding: binding}
	p.comparisons = []ownerComparison{
		{leftRoot: sig.Recv(), leftPath: mutationOwnerIDPath, empty: true},
		{leftRoot: sig.Params().At(0), leftPath: mutationObservedIDPath, empty: true},
		{leftRoot: sig.Recv(), leftPath: mutationOwnerIDPath, rightRoot: sig.Params().At(0), rightPath: mutationObservedIDPath},
		{leftRoot: sig.Recv(), leftPath: ".container", rightRoot: sig.Params().At(0), rightPath: ".Container"},
	}
	if !binding {
		p.comparisons = []ownerComparison{{leftRoot: sig.Recv(), leftPath: mutationBindingPath, nilValue: true}}
	}
	flow := newFunctionFlow(info.decl.Body)
	ast.Inspect(info.decl.Body, func(node ast.Node) bool { return p.checkNode(node, flow) })
}

func (p bindingIdentityProof) checkNode(node ast.Node, flow *functionFlow) bool {
	if _, closure := node.(*ast.FuncLit); closure {
		p.report(node)
		return false
	}
	if call, ok := node.(*ast.CallExpr); ok && !helperLockCall(p.pass, call) && !p.bindingCall(call) {
		p.report(call)
	}
	ret, ok := node.(*ast.ReturnStmt)
	if !ok {
		return true
	}
	for scenario := range 16 {
		p.scenario = scenario
		p.expected = p.want()
		if helperReachable(flow, ret, p.condition) && !p.validReturn(ret) {
			p.report(ret)
			break
		}
	}
	return true
}

func (p bindingIdentityProof) report(node ast.Node) {
	p.pass.Reportf(node.Pos(), "[%s] container identity helper must compare the same binding and prioritize immutable IDs", diagnosticSLC107)
}

func (p bindingIdentityProof) want() bool {
	if !p.binding {
		return p.scenario&1 == 0 && p.scenario&2 != 0
	}
	if p.scenario&3 == 0 {
		return p.scenario&4 != 0
	}
	return p.scenario&8 != 0
}

func (p bindingIdentityProof) validReturn(ret *ast.ReturnStmt) bool {
	return len(ret.Results) == 1 && p.condition(ret.Results[0]) == helperBool(p.expected)
}

func (p bindingIdentityProof) condition(expr ast.Expr) helperTruth {
	return helperBranches(p.pass, p.info.decl.Body, expr, func(atom ast.Expr) helperTruth {
		if call, ok := atom.(*ast.CallExpr); ok && p.bindingCall(call) {
			return helperBool(p.scenario&2 != 0)
		}
		return ownershipAtom(p.pass, p.info.decl.Body, p.resolver, atom, p.comparisons, p.scenario)
	})
}

func (p bindingIdentityProof) bindingCall(call *ast.CallExpr) bool {
	if p.binding || !isLocalMethod(calledFunction(p.pass, call), "workloadBinding", "sameContainer") || len(call.Args) != 1 {
		return false
	}
	sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	return ok && sameResolvedValue(p.resolver, sel.X, p.sig.Recv(), mutationBindingPath) && sameResolvedValue(p.resolver, call.Args[0], p.sig.Params().At(0), "")
}
