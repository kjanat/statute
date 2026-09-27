package statutelifecycle

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/analysis"
)

type ownerComparison struct {
	leftRoot  *types.Var
	leftPath  string
	rightRoot *types.Var
	rightPath string
	nilValue  bool
	empty     bool
}

func checkOwnershipHelper(pass *analysis.Pass, info *functionInfo, current bool) {
	sig, _ := info.fn.Type().(*types.Signature)
	if sig == nil || sig.Recv() == nil || sig.Params().Len() != 1 || !helperHasField(sig.Recv().Type(), mutationBindingField) {
		return
	}
	if current && !helperHasField(sig.Recv().Type(), methodStop) {
		return
	}
	owner, arg := sig.Recv(), sig.Params().At(0)
	comparisons := ownershipComparisons(owner, arg, current)
	resolver := newPathResolver(pass, info.decl.Body)
	flow := newFunctionFlow(info.decl.Body)
	ast.Inspect(info.decl.Body, func(node ast.Node) bool {
		if _, closure := node.(*ast.FuncLit); closure {
			pass.Reportf(node.Pos(), "[%s] ownership helper must expose its immutable owner proof", diagnosticSLC107)
			return false
		}
		ret, ok := node.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		if !ownershipReturnProven(pass, info, resolver, flow, ret, comparisons, current) {
			pass.Reportf(ret.Pos(), "[%s] ownership helper requires the complete stop, binding, key, and immutable ID tuple", diagnosticSLC107)
		}
		return true
	})
}

func ownershipComparisons(owner, arg *types.Var, current bool) []ownerComparison {
	if current {
		return []ownerComparison{
			{leftRoot: arg, leftPath: mutationStopPath, rightRoot: owner, rightPath: mutationStopPath},
			{leftRoot: arg, leftPath: mutationBindingPath, rightRoot: owner, rightPath: mutationBindingPath},
			{leftRoot: owner, leftPath: mutationBindingKeyPath, rightRoot: owner, rightPath: ".bindingKey"},
			{leftRoot: owner, leftPath: ".stop.binding", rightRoot: owner, rightPath: ".bindingKey"},
			{leftRoot: owner, leftPath: mutationContainerIDPath, rightRoot: owner, rightPath: mutationOwnerIDPath},
		}
	}
	return []ownerComparison{
		{leftRoot: arg, nilValue: true},
		{leftRoot: owner, leftPath: mutationStopPath, rightRoot: arg},
		{leftRoot: owner, leftPath: mutationBindingPath, nilValue: true},
		{leftRoot: owner, leftPath: mutationBindingKeyPath, rightRoot: arg, rightPath: mutationBindingPath},
		{leftRoot: owner, leftPath: mutationContainerIDPath, empty: true},
	}
}

func ownershipReturnProven(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, ret *ast.ReturnStmt, comparisons []ownerComparison, current bool) bool {
	result := 1
	if current {
		result = 0
	}
	if len(ret.Results) != result+1 {
		return false
	}
	for scenario := range 1 << len(comparisons) {
		condition := func(expr ast.Expr) helperTruth {
			return helperBranches(pass, info.decl.Body, expr, func(atom ast.Expr) helperTruth {
				return ownershipAtom(pass, info.decl.Body, resolver, atom, comparisons, scenario)
			})
		}
		if !helperReachable(flow, ret, condition) || condition(ret.Results[result])&helperTrue == 0 {
			continue
		}
		if !ownershipMatches(comparisons, scenario) {
			return false
		}
		if !current && !ownershipCaptureTuple(pass, info, resolver, ret.Results[0]) {
			return false
		}
	}
	return true
}

func ownershipMatches(comparisons []ownerComparison, scenario int) bool {
	for i, comparison := range comparisons {
		wantEqual := !comparison.nilValue && !comparison.empty
		if (scenario&(1<<i) != 0) != wantEqual {
			return false
		}
	}
	return true
}

func ownershipAtom(pass *analysis.Pass, body *ast.BlockStmt, resolver *pathResolver, expr ast.Expr, comparisons []ownerComparison, scenario int) helperTruth {
	binary, ok := expr.(*ast.BinaryExpr)
	if !ok || (binary.Op != token.EQL && binary.Op != token.NEQ) {
		return helperUnknown
	}
	for _, pair := range [][2]ast.Expr{{binary.X, binary.Y}, {binary.Y, binary.X}} {
		left := stableDefinitionExpr(pass, body, pair[0], 0)
		right := stableDefinitionExpr(pass, body, pair[1], 0)
		for i, comparison := range comparisons {
			if !sameResolvedValue(resolver, left, comparison.leftRoot, comparison.leftPath) {
				continue
			}
			if comparison.matchesRight(pass, resolver, right) {
				return helperBool((scenario&(1<<i) != 0) == (binary.Op == token.EQL))
			}
		}
	}
	return helperUnknown
}

func (c ownerComparison) matchesRight(pass *analysis.Pass, resolver *pathResolver, expr ast.Expr) bool {
	if c.rightRoot != nil {
		return sameResolvedValue(resolver, expr, c.rightRoot, c.rightPath)
	}
	return c.nilValue && isNil(pass, expr) || c.empty && helperEmpty(pass, expr)
}

func ownershipCaptureTuple(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, expr ast.Expr) bool {
	sig, _ := info.fn.Type().(*types.Signature)
	expr = helperStableRecord(pass, info.decl.Body, resolver, expr)
	if expr == nil {
		return false
	}
	literal, ok := ast.Unparen(expr).(*ast.CompositeLit)
	if !ok || !isNamedPackageType(pass.TypesInfo.TypeOf(literal), "workloadStopOwnership") {
		return false
	}
	want := map[string]aliasTarget{
		methodStop: {root: sig.Params().At(0)}, mutationBindingField: {root: sig.Recv(), path: mutationBindingPath},
		"bindingKey": {root: sig.Recv(), path: mutationBindingKeyPath}, mutationContainerIDField: {root: sig.Recv(), path: mutationContainerIDPath},
		"service": {root: sig.Recv(), path: ".service"}, "containerName": {root: sig.Recv(), path: ".binding.container"},
	}
	for _, element := range literal.Elts {
		field, ok := element.(*ast.KeyValueExpr)
		if !ok {
			return false
		}
		key, ok := field.Key.(*ast.Ident)
		if !ok {
			return false
		}
		if target, required := want[key.Name]; required {
			value := stableDefinitionExpr(pass, info.decl.Body, field.Value, 0)
			if !sameResolvedValue(resolver, value, target.root, target.path) {
				return false
			}
			delete(want, key.Name)
		}
	}
	return len(want) == 0
}

type attemptProof struct {
	pass       *analysis.Pass
	info       *functionInfo
	resolver   *pathResolver
	stopErr    *types.Var
	inspectErr *types.Var
	inspection *types.Var
	stopOK     bool
	missing    bool
	ambiguous  bool
	inspectOK  bool
	running    bool
	inspect404 bool
}

func checkStopResultHelper(pass *analysis.Pass, info *functionInfo) {
	sig, _ := info.fn.Type().(*types.Signature)
	if sig == nil || sig.Recv() == nil || sig.Params().Len() != 1 || !helperHasField(sig.Params().At(0).Type(), "terminal") {
		return
	}
	resolver := newPathResolver(pass, info.decl.Body)
	flow := newFunctionFlow(info.decl.Body)
	ast.Inspect(info.decl.Body, func(node ast.Node) bool {
		ret, ok := node.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		if !stopResultReturnProven(pass, info, resolver, flow, sig, ret) {
			pass.Reportf(ret.Pos(), "[%s] terminal replay requires the same owned stop and its recorded terminal result", diagnosticSLC108)
		}
		return true
	})
}

func stopResultReturnProven(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, sig *types.Signature, ret *ast.ReturnStmt) bool {
	if len(ret.Results) != 3 {
		return false
	}
	comparisons := []ownerComparison{{leftRoot: sig.Recv(), leftPath: mutationStopPath, rightRoot: sig.Params().At(0)}}
	for scenario := range 4 {
		current, terminal := scenario&1 != 0, scenario&2 != 0
		condition := func(expr ast.Expr) helperTruth {
			return helperBranches(pass, info.decl.Body, expr, func(atom ast.Expr) helperTruth {
				if sameResolvedValue(resolver, atom, sig.Params().At(0), ".terminal") {
					return helperBool(terminal)
				}
				return ownershipAtom(pass, info.decl.Body, resolver, atom, comparisons, scenario&1)
			})
		}
		if !helperReachable(flow, ret, condition) {
			continue
		}
		if condition(ret.Results[2])&helperTrue != 0 && !current {
			return false
		}
		if condition(ret.Results[1])&helperTrue != 0 {
			result := stableDefinitionExpr(pass, info.decl.Body, ret.Results[0], 0)
			if !terminalReplayResult(current, terminal, resolver, result, sig.Params().At(0)) {
				return false
			}
		}
	}
	return true
}

func terminalReplayResult(current, terminal bool, resolver *pathResolver, result ast.Expr, stop *types.Var) bool {
	return current && terminal && sameResolvedValue(resolver, result, stop, mutationResultPath)
}

func checkAttemptClassification(pass *analysis.Pass, info *functionInfo, parents map[ast.Node]ast.Node) {
	if pass.Pkg.Scope().Lookup("workloadStopAmbiguous") == nil {
		return
	}
	proof := attemptProof{pass: pass, info: info, resolver: newPathResolver(pass, info.decl.Body)}
	flow := newFunctionFlow(info.decl.Body)
	stops := 0
	ast.Inspect(info.decl.Body, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok {
			if proof.checkCall(call, flow, parents) {
				stops++
			}
		}
		proof.collectAssignment(node)
		return true
	})
	if stops != 1 {
		pass.Reportf(info.decl.Name.Pos(), "[%s] one stop attempt must issue exactly one Docker mutation", diagnosticSLC108)
	}
	proof.checkReturns(flow)
}

func (p *attemptProof) collectAssignment(node ast.Node) {
	assign, ok := node.(*ast.AssignStmt)
	if !ok || len(assign.Rhs) != 1 {
		return
	}
	call, ok := ast.Unparen(assign.Rhs[0]).(*ast.CallExpr)
	if !ok {
		return
	}
	if isMethod(calledFunction(p.pass, call), dockerPackagePath, "Client", "StopContainer") && len(assign.Lhs) == 1 {
		p.stopErr = helperDefinedVar(p.pass, assign.Lhs[0])
	}
	if isMethod(calledFunction(p.pass, call), dockerPackagePath, "Client", "InspectContainer") && len(assign.Lhs) == 2 {
		p.inspection, p.inspectErr = helperDefinedVar(p.pass, assign.Lhs[0]), helperDefinedVar(p.pass, assign.Lhs[1])
	}
}

func (p attemptProof) checkCall(call *ast.CallExpr, flow *functionFlow, parents map[ast.Node]ast.Node) bool {
	if isMethod(calledFunction(p.pass, call), dockerPackagePath, "Client", "StopContainer") {
		if helperRepeatedCall(call, flow) {
			p.pass.Reportf(call.Pos(), "[%s] one stop attempt may not repeat a Docker mutation before preserving its outcome", diagnosticSLC108)
		}
		return true
	}
	if isMethod(calledFunction(p.pass, call), dockerPackagePath, "Client", "InspectContainer") && !p.inspectionProven(call, flow, parents) {
		p.pass.Reportf(call.Pos(), "[%s] stop inspection requires the same provider, immutable operation target, and bounded context", diagnosticSLC108)
	}
	return false
}

func (p attemptProof) checkReturns(flow *functionFlow) {
	ast.Inspect(p.info.decl.Body, func(node ast.Node) bool {
		ret, ok := node.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		for scenario := range 64 {
			p.stopOK, p.missing, p.ambiguous = scenario&1 != 0, scenario&2 != 0, scenario&4 != 0
			p.inspectOK, p.running, p.inspect404 = scenario&8 != 0, scenario&16 != 0, scenario&32 != 0
			if helperReachable(flow, ret, p.condition) && !p.validResult(ret) {
				p.pass.Reportf(ret.Pos(), "[%s] Docker stop outcome must preserve ambiguity unless the same call or inspection proves termination", diagnosticSLC108)
				break
			}
		}
		return true
	})
}

func helperRepeatedCall(call *ast.CallExpr, flow *functionFlow) bool {
	point, ok := flow.point(call)
	if !ok {
		return true
	}
	for _, successor := range point.block.Succs {
		if flow.blockReaches(successor, point.block) {
			return true
		}
	}
	return false
}

func (p attemptProof) inspectionProven(call *ast.CallExpr, flow *functionFlow, parents map[ast.Node]ast.Node) bool {
	sig, _ := p.info.fn.Type().(*types.Signature)
	if sig == nil || sig.Recv() == nil || sig.Params().Len() != 3 || len(call.Args) != 2 || !helperSynchronous(call, p.info.decl.Body, parents) {
		return false
	}
	sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok || !sameResolvedValue(p.resolver, sel.X, sig.Recv(), ".client") || !validMutationTarget(p.pass, sig, stableDefinitionExpr(p.pass, p.info.decl.Body, call.Args[1], 0), p.resolver) {
		return false
	}
	bound, ok := timeoutBindingFor(p.pass, p.info.decl.Body, call.Args[0])
	return ok && p.inspectionContext(bound, sig, call, flow, parents)
}

func helperSynchronous(call *ast.CallExpr, body *ast.BlockStmt, parents map[ast.Node]ast.Node) bool {
	return !insideDeferredOrGo(call, body, parents) && !enclosedByFuncLiteral(call, body, parents)
}

func (p attemptProof) inspectionContext(bound timeoutBinding, sig *types.Signature, call *ast.CallExpr, flow *functionFlow, parents map[ast.Node]ast.Node) bool {
	return flow.dominates(bound.assignment, call) && sameResolvedValue(p.resolver, bound.parent, sig.Params().At(0), "") && helperConstant(p.pass, bound.timeout, "workloadProbeTimeout") && hasCancellationProof(p.pass, p.info.decl.Body, call, bound.cancel, flow, parents)
}

func (p attemptProof) value(expr ast.Expr, root *types.Var, path string) bool {
	return root != nil && sameResolvedValue(p.resolver, expr, root, path)
}

func (p attemptProof) condition(expr ast.Expr) helperTruth {
	return helperBranches(p.pass, p.info.decl.Body, expr, p.atom)
}

func (p attemptProof) atom(expr ast.Expr) helperTruth {
	if p.value(expr, p.inspection, mutationRunningPath) {
		return helperBool(p.running)
	}
	if binary, ok := expr.(*ast.BinaryExpr); ok {
		return p.errorComparison(binary)
	}
	return p.classification(expr)
}

func (p attemptProof) errorComparison(binary *ast.BinaryExpr) helperTruth {
	if binary.Op == token.EQL || binary.Op == token.NEQ {
		for _, pair := range [][2]ast.Expr{{binary.X, binary.Y}, {binary.Y, binary.X}} {
			if !isNil(p.pass, pair[1]) {
				continue
			}
			if p.value(pair[0], p.stopErr, "") {
				return helperBool(p.stopOK == (binary.Op == token.EQL))
			}
			if p.value(pair[0], p.inspectErr, "") {
				return helperBool(p.inspectOK == (binary.Op == token.EQL))
			}
		}
	}
	return helperUnknown
}

func (p attemptProof) classification(expr ast.Expr) helperTruth {
	call, ok := expr.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return helperUnknown
	}
	if isPackageFunction(calledFunction(p.pass, call), dockerPackagePath, "LifecycleContainerMissing") {
		if p.value(call.Args[0], p.stopErr, "") {
			return helperBool(p.missing)
		}
		if p.value(call.Args[0], p.inspectErr, "") {
			return helperBool(p.inspect404)
		}
	}
	if isPackageFunction(calledFunction(p.pass, call), dockerPackagePath, "LifecycleOutcomeAmbiguous") && p.value(call.Args[0], p.stopErr, "") {
		return helperBool(p.ambiguous)
	}
	return helperUnknown
}

func (p attemptProof) validResult(ret *ast.ReturnStmt) bool {
	if len(ret.Results) != 1 {
		return false
	}
	expr := stableDefinitionExpr(p.pass, p.info.decl.Body, ret.Results[0], 0)
	literal, ok := ast.Unparen(expr).(*ast.CompositeLit)
	if !ok {
		return false
	}
	want := p.expectedResult()
	for _, element := range literal.Elts {
		field, ok := element.(*ast.KeyValueExpr)
		if !ok {
			return false
		}
		key, ok := field.Key.(*ast.Ident)
		if ok && key.Name == mutationResultField {
			value := stableDefinitionExpr(p.pass, p.info.decl.Body, field.Value, 0)
			return helperConstant(p.pass, value, "workloadStopAmbiguous") || helperConstant(p.pass, value, want)
		}
	}
	constantResult, _ := p.pass.Pkg.Scope().Lookup(want).(*types.Const)
	return constantResult != nil && constant.Sign(constantResult.Val()) == 0
}

func (p attemptProof) expectedResult() string {
	switch {
	case p.stopOK || p.missing:
		return "workloadStopSucceeded"
	case !p.ambiguous:
		return "workloadStopRejected"
	case p.inspectOK && !p.running || p.inspect404:
		return "workloadStopSucceeded"
	default:
		return "workloadStopAmbiguous"
	}
}
