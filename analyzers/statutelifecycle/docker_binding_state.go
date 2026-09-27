package statutelifecycle

import (
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/analysis"
)

func mutationComposite(expr ast.Expr) *ast.CompositeLit {
	if address, ok := ast.Unparen(expr).(*ast.UnaryExpr); ok && address.Op == token.AND {
		expr = address.X
	}
	literal, _ := ast.Unparen(expr).(*ast.CompositeLit)
	return literal
}

func protectedBindingStorage(pass *analysis.Pass, resolver *pathResolver, expr ast.Expr) bool {
	if _, fresh := ast.Unparen(expr).(*ast.CompositeLit); fresh {
		return false
	}
	if protectedStateField(pass, resolver, expr, "dockerProvider", bindingCounterField) {
		return true
	}
	for _, field := range []string{mutationBindingField, "ref"} {
		if protectedStateField(pass, resolver, expr, "workloadStop", field) {
			return true
		}
	}
	for _, field := range []string{"key", mutationContainerIDField, "container"} {
		if protectedStateField(pass, resolver, expr, "workloadBinding", field) {
			return true
		}
	}
	return protectedStateField(pass, resolver, expr, "workload", mutationBindingField) || isNamedPackageValueType(pass.TypesInfo.TypeOf(expr), "workloadBinding")
}

func checkBindingWrite(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, node ast.Node, lhs, rhs ast.Expr, local bool) {
	if !protectedBindingStorage(pass, resolver, lhs) {
		return
	}
	if id, ok := ast.Unparen(lhs).(*ast.Ident); ok && pass.TypesInfo.Defs[id] != nil {
		return
	}
	if local && bindingPersistenceWrite(pass, info, resolver, node, lhs) {
		return
	}
	valid := local && (bindingObservationWrite(pass, info, resolver, flow, node, lhs, rhs) || bindingConstructionWrite(pass, info, resolver, lhs, rhs) || bindingCounterWrite(info, resolver, node, lhs))
	if !valid {
		pass.Reportf(node.Pos(), "[%s] immutable mutation binding may change only through canonical observation or binding construction", diagnosticSLC105)
	}
}

func bindingPersistenceWrite(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, node ast.Node, lhs ast.Expr) bool {
	_, assignment := node.(*ast.AssignStmt)
	return assignment && isLocalMethod(info.fn, "dockerProvider", "persistOwnedStop") && protectedStateField(pass, resolver, lhs, "workloadStop", "ref")
}

func bindingObservationWrite(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, node ast.Node, lhs, rhs ast.Expr) bool {
	rhs = stableDefinitionExpr(pass, info.decl.Body, rhs, 0)
	sig, _ := info.fn.Type().(*types.Signature)
	if !isLocalMethod(info.fn, "workloadBinding", "observe") || sig == nil || sig.Params().Len() != 1 {
		return false
	}
	if sameBindingStorage(resolver, lhs, sig.Recv(), ".container") {
		return sameResolvedValue(resolver, rhs, sig.Params().At(0), ".Container")
	}
	if !sameBindingStorage(resolver, lhs, sig.Recv(), mutationOwnerIDPath) || !sameResolvedValue(resolver, rhs, sig.Params().At(0), mutationObservedIDPath) {
		return false
	}
	condition := func(expr ast.Expr) helperTruth {
		return helperBranches(pass, info.decl.Body, expr, func(atom ast.Expr) helperTruth {
			return ownershipAtom(pass, info.decl.Body, resolver, atom, []ownerComparison{{leftRoot: sig.Params().At(0), leftPath: mutationObservedIDPath, empty: true}}, 1)
		})
	}
	return !helperReachable(flow, node, condition)
}

func bindingConstructionWrite(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, lhs, rhs ast.Expr) bool {
	sig, _ := info.fn.Type().(*types.Signature)
	if !isLocalMethod(info.fn, "dockerProvider", "newWorkloadBindingLocked") || sig == nil || sig.Params().Len() != 2 || !sameBindingStorage(resolver, lhs, sig.Params().At(0), mutationBindingPath) {
		return false
	}
	literal := mutationComposite(stableDefinitionExpr(pass, info.decl.Body, rhs, 0))
	if !helperLiteralType(pass, literal, "workloadBinding") {
		return false
	}
	if !bindingLiteralService(resolver, literal, sig.Params().At(1)) {
		return false
	}
	call, ok := ast.Unparen(bindingLiteralField(literal, "key")).(*ast.CallExpr)
	if !ok || !isLocalMethod(calledFunction(pass, call), "dockerProvider", "nextWorkloadBindingLocked") {
		return false
	}
	sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	return ok && sameResolvedValue(resolver, sel.X, sig.Recv(), "")
}

func bindingLiteralService(resolver *pathResolver, literal *ast.CompositeLit, service *types.Var) bool {
	return sameResolvedValue(resolver, bindingLiteralField(literal, mutationContainerIDField), service, mutationObservedIDPath) && sameResolvedValue(resolver, bindingLiteralField(literal, "container"), service, ".Container")
}

func sameBindingStorage(resolver *pathResolver, expr ast.Expr, want *types.Var, field string) bool {
	root, path, ok := resolver.resolveExpr(expr)
	return ok && root == want && path == field
}

func bindingLiteralField(literal *ast.CompositeLit, name string) ast.Expr {
	for _, element := range literal.Elts {
		key, value, ok := helperLiteralField(element)
		if ok && key == name {
			return value
		}
	}
	return nil
}

func checkBindingReference(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, parents map[ast.Node]ast.Node, sel *ast.SelectorExpr) {
	fn := selectedFunction(pass, sel)
	if isLocalInterfaceMethod(fn, "newWorkloadBindingLocked") || isLocalInterfaceMethod(fn, "observe") {
		pass.Reportf(sel.Pos(), "[%s] binding identity changes require concrete typed boundaries", diagnosticSLC105)
		return
	}
	constructor := isLocalMethod(fn, "dockerProvider", "newWorkloadBindingLocked")
	observe := isLocalMethod(fn, "workloadBinding", "observe")
	if !constructor && !observe {
		return
	}
	call := directSelectorCall(sel, parents)
	if call == nil || !helperSynchronous(call, info.decl.Body, parents) || !bindingIngress(pass, info, resolver, flow, call, constructor) {
		pass.Reportf(sel.Pos(), "[%s] binding identity changes require the canonical same-container or supersession guard", diagnosticSLC105)
	}
}

func bindingIngress(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, call *ast.CallExpr, constructor bool) bool {
	sig, _ := info.fn.Type().(*types.Signature)
	if !isLocalMethod(info.fn, "dockerProvider", "bindWorkloadContainerLocked") || sig == nil || sig.Params().Len() != 2 {
		return false
	}
	if !bindingIngressArguments(resolver, sig, call, constructor) {
		return false
	}
	for scenario := range 4 {
		absent, same := scenario&1 != 0, scenario&2 != 0
		condition := bindingIngressCondition(pass, info, resolver, sig, scenario)
		if !helperReachable(flow, call, condition) {
			continue
		}
		if !bindingIngressScenario(pass, info, resolver, flow, call, sig, constructor, absent, same) {
			return false
		}
	}
	return true
}

func bindingIngressScenario(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, call *ast.CallExpr, sig *types.Signature, constructor, absent, same bool) bool {
	if !constructor {
		return !absent && same
	}
	return absent || !same && bindingSupersessionDominates(pass, info, resolver, flow, call, sig.Params().At(0))
}

func bindingIngressArguments(resolver *pathResolver, sig *types.Signature, call *ast.CallExpr, constructor bool) bool {
	sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if constructor {
		return len(call.Args) == 2 && sameResolvedValue(resolver, sel.X, sig.Recv(), "") && sameResolvedValue(resolver, call.Args[0], sig.Params().At(0), "") && sameResolvedValue(resolver, call.Args[1], sig.Params().At(1), "")
	}
	return len(call.Args) == 1 && sameResolvedValue(resolver, sel.X, sig.Params().At(0), mutationBindingPath) && sameResolvedValue(resolver, call.Args[0], sig.Params().At(1), "")
}

func bindingIngressCondition(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, sig *types.Signature, scenario int) func(ast.Expr) helperTruth {
	return func(expr ast.Expr) helperTruth {
		return helperBranches(pass, info.decl.Body, expr, func(atom ast.Expr) helperTruth {
			if call, ok := atom.(*ast.CallExpr); ok && bindingSameCall(pass, resolver, sig, call) {
				return helperBool(scenario&2 != 0)
			}
			return ownershipAtom(pass, info.decl.Body, resolver, atom, []ownerComparison{{leftRoot: sig.Params().At(0), leftPath: mutationBindingPath, nilValue: true}}, scenario)
		})
	}
}

func bindingSameCall(pass *analysis.Pass, resolver *pathResolver, sig *types.Signature, call *ast.CallExpr) bool {
	if !isLocalMethod(calledFunction(pass, call), "workload", "sameContainerLocked") || len(call.Args) != 1 {
		return false
	}
	sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	return ok && sameResolvedValue(resolver, sel.X, sig.Params().At(0), "") && sameResolvedValue(resolver, call.Args[0], sig.Params().At(1), "")
}

func bindingSupersessionDominates(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, target ast.Node, owner *types.Var) bool {
	valid := false
	ast.Inspect(info.decl.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || !isLocalMethod(calledFunction(pass, call), "workload", "supersedeBindingLocked") || !flow.dominates(call, target) {
			return true
		}
		sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
		valid = valid || ok && sameResolvedValue(resolver, sel.X, owner, "")
		return true
	})
	return valid
}
