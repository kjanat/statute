package statutelifecycle

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/cfg"
)

// proofValues supplies only the small architectural predicates under test.
// Unknown expressions explore both CFG edges; an unknown is never evidence.
type proofValues map[aliasTarget]int64

const mutationUncertainPath = ".uncertain"

func proofValue(pass *analysis.Pass, body *ast.BlockStmt, resolver *pathResolver, expr ast.Expr, values proofValues) (int64, bool) {
	if expr == nil {
		return 0, false
	}
	if value, ok := proofLiteral(pass, expr); ok {
		return value, true
	}
	if root, path, ok := resolver.resolveExpr(expr); ok {
		if value, exists := values[aliasTarget{root: root, path: path}]; exists && proofStorageStable(resolver, root, path) {
			return value, true
		}
	}
	return proofExpression(pass, body, resolver, expr, values)
}

func proofStorageStable(resolver *pathResolver, root *types.Var, path string) bool {
	return !resolver.pathInvalidated(root, path) || path == mutationUncertainPath || path == mutationStopPath
}

func proofLiteral(pass *analysis.Pass, expr ast.Expr) (int64, bool) {
	if isNil(pass, expr) {
		return 0, true
	}
	value := pass.TypesInfo.Types[expr].Value
	if value == nil {
		return 0, false
	}
	switch {
	case value.Kind() == constant.Bool:
		return proofBoolean(constant.BoolVal(value)), true
	case value.Kind() == constant.Int:
		return constant.Int64Val(value)
	}
	return 0, false
}
func proofBoolean(value bool) int64 {
	if value {
		return 1
	}
	return 0
}
func proofExpression(pass *analysis.Pass, body *ast.BlockStmt, resolver *pathResolver, expr ast.Expr, values proofValues) (int64, bool) {
	switch n := ast.Unparen(expr).(type) {
	case *ast.UnaryExpr:
		v, ok := proofValue(pass, body, resolver, n.X, values)
		if n.Op == token.NOT && ok {
			return proofBoolean(v == 0), true
		}
	case *ast.BinaryExpr:
		a, aok := proofValue(pass, body, resolver, n.X, values)
		b, bok := proofValue(pass, body, resolver, n.Y, values)
		return proofBinary(n.Op, a, aok, b, bok)
	case *ast.Ident:
		definition := stableDefinitionExpr(pass, body, expr, 0)
		if definition != expr {
			return proofValue(pass, body, resolver, definition, values)
		}
	}
	return 0, false
}
func proofBinary(op token.Token, a int64, aok bool, b int64, bok bool) (int64, bool) {
	if op == token.LAND {
		return proofAnd(a, aok, b, bok)
	}
	if op == token.LOR {
		v, ok := proofAnd(proofBoolean(a == 0), aok, proofBoolean(b == 0), bok)
		return proofBoolean(v == 0), ok
	}
	if !aok || !bok {
		return 0, false
	}
	if op == token.EQL {
		return proofBoolean(a == b), true
	}
	if op == token.NEQ {
		return proofBoolean(a != b), true
	}
	return 0, false
}
func proofAnd(a int64, aok bool, b int64, bok bool) (int64, bool) {
	if (aok && a == 0) || (bok && b == 0) {
		return 0, true
	}
	if aok && bok {
		return 1, true
	}
	return 0, false
}

func proofReaches(pass *analysis.Pass, body *ast.BlockStmt, resolver *pathResolver, flow *functionFlow, target ast.Node, values proofValues) bool {
	return proofReachesFrom(pass, body, resolver, flow, flow.graph.Blocks[0], target, values)
}

func proofReachesFrom(pass *analysis.Pass, body *ast.BlockStmt, resolver *pathResolver, flow *functionFlow, start *cfg.Block, target ast.Node, values proofValues) bool {
	point, ok := flow.point(target)
	if !ok {
		return true
	}
	queue := []*cfg.Block{start}
	seen := make(map[*cfg.Block]bool)
	for len(queue) != 0 {
		block := queue[0]
		queue = queue[1:]
		if !block.Live || seen[block] {
			continue
		}
		seen[block] = true
		reached, terminated := proofBlockTarget(pass, block, point)
		if reached {
			return true
		}
		if terminated {
			continue
		}
		queue = append(queue, proofSuccessors(pass, body, resolver, block, values)...)
	}
	return false
}
func proofBlockTarget(pass *analysis.Pass, block *cfg.Block, point flowPoint) (bool, bool) {
	for index, node := range block.Nodes {
		if block == point.block && index == point.index {
			return true, false
		}
		if nodeHasCall(node, func(call *ast.CallExpr) bool { return proofPanic(pass, call) }) {
			return false, true
		}
	}
	return false, false
}
func proofPanic(pass *analysis.Pass, call *ast.CallExpr) bool {
	id, ok := ast.Unparen(call.Fun).(*ast.Ident)
	if !ok {
		return false
	}
	builtin, _ := pass.TypesInfo.Uses[id].(*types.Builtin)
	return builtin != nil && builtin.Name() == "panic"
}
func proofSuccessors(pass *analysis.Pass, body *ast.BlockStmt, resolver *pathResolver, block *cfg.Block, values proofValues) []*cfg.Block {
	if len(block.Succs) != 2 || len(block.Nodes) == 0 {
		return block.Succs
	}
	expr, ok := block.Nodes[len(block.Nodes)-1].(ast.Expr)
	if !ok {
		return block.Succs
	}
	v, known := proofValue(pass, body, resolver, expr, values)
	if !known {
		return block.Succs
	}
	if v != 0 {
		return block.Succs[:1]
	}
	return block.Succs[1:]
}

func packageInt(pass *analysis.Pass, name string) (int64, bool) {
	c, ok := pass.Pkg.Scope().Lookup(name).(*types.Const)
	if !ok {
		return 0, false
	}
	return constant.Int64Val(c.Val())
}

func protectedStateField(pass *analysis.Pass, resolver *pathResolver, expr ast.Expr, owner, field string) bool {
	if isFieldSelection(pass, expr, owner, field) {
		return true
	}
	root, path, ok := resolver.resolveExpr(expr)
	return ok && ((isNamedPackageType(root.Type(), owner) && path == "."+field) ||
		(owner == "workloadStop" && isNamedPackageType(root.Type(), "workload") && path == ".stop."+field))
}

func checkMutationState(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, parents map[ast.Node]ast.Node) {
	ast.Inspect(info.decl.Body, func(node ast.Node) bool {
		checkStateNode(pass, info, resolver, flow, parents, node)
		return true
	})
	checkAmbiguousReturn(pass, info, resolver, flow)
}
func checkStateNode(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, parents map[ast.Node]ast.Node, node ast.Node) {
	switch n := node.(type) {
	case *ast.CompositeLit:
		checkStateConstruction(pass, info, n)
		checkWorkloadConstruction(pass, info, resolver, n)
	case *ast.IncDecStmt:
		checkStateWrite(pass, info, resolver, flow, parents, n, n.X, nil)
	case *ast.RangeStmt:
		checkStateRange(pass, info, resolver, flow, parents, n)
	case *ast.AssignStmt:
		checkStateAssignment(pass, info, resolver, flow, parents, n)
	case *ast.UnaryExpr:
		checkStateAddress(pass, resolver, n)
	case *ast.Ident:
		checkStateAlias(pass, resolver, parents, n)
	case *ast.CallExpr:
		checkStateCall(pass, info, resolver, flow, parents, n)
	case *ast.SelectorExpr:
		checkStateReference(pass, parents, n)
		checkBindingReference(pass, info, resolver, flow, parents, n)
	}
}
func checkStateRange(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, parents map[ast.Node]ast.Node, n *ast.RangeStmt) {
	if n.Tok != token.ASSIGN {
		return
	}
	for _, lhs := range []ast.Expr{n.Key, n.Value} {
		if lhs != nil {
			checkStateWrite(pass, info, resolver, flow, parents, n, lhs, nil)
		}
	}
}
func checkStateAssignment(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, parents map[ast.Node]ast.Node, n *ast.AssignStmt) {
	for i, lhs := range n.Lhs {
		var rhs ast.Expr
		if i < len(n.Rhs) {
			rhs = n.Rhs[i]
		}
		checkStateWrite(pass, info, resolver, flow, parents, n, lhs, rhs)
	}
}
func checkStateAlias(pass *analysis.Pass, resolver *pathResolver, parents map[ast.Node]ast.Node, n *ast.Ident) {
	v, _ := pass.TypesInfo.Uses[n].(*types.Var)
	if v == nil || !resolver.addrAliases[v] || resolver.aliasRHS[n] || !protectedStorage(pass, resolver, n) {
		return
	}
	switch parents[n].(type) {
	case *ast.StarExpr, *ast.SelectorExpr:
		return
	}
	pass.Reportf(n.Pos(), "[SLC107] protected mutation state address alias may not escape")
}
func checkStateCall(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, parents map[ast.Node]ast.Node, n *ast.CallExpr) {
	fn := calledFunction(pass, n)
	if isLocalMethod(fn, "workload", "applyStopAttempt") && !synchronousEvidenceProven(pass, info, resolver, flow, parents, n) {
		pass.Reportf(n.Pos(), "[SLC108] stop attempt application requires classified Docker evidence or guarded terminal replay")
	}
	if isLocalMethod(fn, "dockerProvider", "recordObservedStopLocked") && !observedStopCallProven(pass, info, resolver, flow, n) {
		pass.Reportf(n.Pos(), "[SLC108] observed terminal evidence requires canonical stopped or missing observation with no issued call")
	}
	if isLocalMethod(fn, "dockerProvider", "observeStoppedWorkloadLocked") && !stoppedObservationProven(pass, info, resolver, flow, n) {
		pass.Reportf(n.Pos(), "[SLC108] stopped observation requires the non-running discovery edge")
	}
	if isLocalInterfaceMethod(fn, "transitionLocked") {
		pass.Reportf(n.Pos(), "[SLC109] raw phase transition must use the concrete workload method")
	}
	checkRawTransitionCall(pass, info, resolver, flow, parents, n)
	if isLocalMethod(fn, "mutationRegistry", "delete") && isLocalMethod(info.fn, "workload", "applyStopAttempt") {
		checkTerminalEligibility(pass, info, resolver, flow, n)
	}

}
func checkRawTransitionCall(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, parents map[ast.Node]ast.Node, n *ast.CallExpr) {
	fn := calledFunction(pass, n)
	if isLocalMethod(fn, "workload", "transitionLocked") {
		allowed := isLocalMethod(info.fn, "workload", "settleStopLocked") || isLocalMethod(info.fn, "workload", "supersedeBindingLocked")
		sig, _ := info.fn.Type().(*types.Signature)
		sel, _ := ast.Unparen(n.Fun).(*ast.SelectorExpr)
		allowed = allowed && sig != nil && sel != nil && sameResolvedValue(resolver, sel.X, sig.Recv(), "")
		if isLocalMethod(info.fn, "workload", "toLocked") {
			allowed = phaseGuardProven(pass, info, resolver, flow, n)
		}
		if !allowed || enclosedByFuncLiteral(n, info.decl.Body, parents) || insideDeferredOrGo(n, info.decl.Body, parents) {
			pass.Reportf(n.Pos(), "[SLC109] raw phase transition requires the mutation quarantine guard or canonical settlement")
		}
	}

}
func checkStateReference(pass *analysis.Pass, parents map[ast.Node]ast.Node, n *ast.SelectorExpr) {
	if checkStateInterfaceReference(pass, parents, n) {
		return
	}
	checkConcreteStateReference(pass, parents, n)
}

func checkStateInterfaceReference(pass *analysis.Pass, parents map[ast.Node]ast.Node, n *ast.SelectorExpr) bool {
	if protectedEvidenceInterface(selectedFunction(pass, n)) {
		pass.Reportf(n.Pos(), "[SLC108] mutation evidence requires concrete typed boundaries")
		return true
	}
	if isLocalInterfaceMethod(selectedFunction(pass, n), "transitionLocked") && directSelectorCall(n, parents) == nil {
		pass.Reportf(n.Pos(), "[SLC109] raw phase transition may not escape through an interface")
	}
	return false
}

func checkConcreteStateReference(pass *analysis.Pass, parents map[ast.Node]ast.Node, n *ast.SelectorExpr) {
	if isLocalMethod(selectedFunction(pass, n), "workload", "applyStopAttempt") && directSelectorCall(n, parents) == nil {
		pass.Reportf(n.Pos(), "[SLC108] attempt evidence boundary may not escape")
	}
	if isLocalMethod(selectedFunction(pass, n), "workload", "transitionLocked") && directSelectorCall(n, parents) == nil {
		pass.Reportf(n.Pos(), "[SLC109] raw phase transition may not escape its canonical boundary")
	}
	if (isLocalMethod(selectedFunction(pass, n), "dockerProvider", "recordObservedStopLocked") || isLocalMethod(selectedFunction(pass, n), "dockerProvider", "observeStoppedWorkloadLocked")) && directSelectorCall(n, parents) == nil {
		pass.Reportf(n.Pos(), "[SLC108] terminal observation boundary may not escape")
	}
}

func protectedEvidenceInterface(fn *types.Func) bool {
	return isLocalInterfaceMethod(fn, "applyStopAttempt") || isLocalInterfaceMethod(fn, "recordObservedStopLocked") || isLocalInterfaceMethod(fn, "observeStoppedWorkloadLocked")
}

func checkStateConstruction(pass *analysis.Pass, info *functionInfo, literal *ast.CompositeLit) {
	if isNamedPackageType(pass.TypesInfo.TypeOf(literal), "workloadStop") {
		for _, element := range literal.Elts {
			kv, ok := element.(*ast.KeyValueExpr)
			if !ok {
				pass.Reportf(literal.Pos(), "[SLC108] mutation construction requires explicit state fields")
				return
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			if key.Name == "terminal" || key.Name == mutationResultField {
				pass.Reportf(kv.Pos(), "[SLC108] mutation construction may not fabricate terminal evidence")
			}
			if key.Name == "persisted" && !isLocalMethod(info.fn, "dockerProvider", "restoreMutationRecords") {
				pass.Reportf(kv.Pos(), "[SLC106] persisted mutation construction requires durable recovery")
			}
		}
	}
}
func checkWorkloadConstruction(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, literal *ast.CompositeLit) {
	if isNamedPackageType(pass.TypesInfo.TypeOf(literal), "workload") {
		var stop, phase ast.Expr
		for _, element := range literal.Elts {
			kv, ok := element.(*ast.KeyValueExpr)
			if !ok {
				pass.Reportf(literal.Pos(), "[SLC109] workload construction requires explicit state fields")
				return
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			if key.Name == methodStop {
				stop = kv.Value
			}
			if key.Name == "phase" {
				phase = kv.Value
			}
		}
		if stop != nil && !isNilValue(pass, info.decl.Body, stop) {
			if !quarantinePhase(pass, info, resolver, phase) {
				pass.Reportf(literal.Pos(), "[SLC109] a constructed mutation owner must remain in quarantine")
			}
		}
	}
}

func quarantinePhase(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, phase ast.Expr) bool {
	v, known := proofValue(pass, info.decl.Body, resolver, phase, nil)
	issued, iok := packageInt(pass, "workloadStopIssued")
	unknown, uok := packageInt(pass, "workloadStopUnknown")
	return known && iok && uok && (v == issued || v == unknown)
}

func protectedStorage(pass *analysis.Pass, resolver *pathResolver, expr ast.Expr) bool {
	if protectedBindingStorage(pass, resolver, expr) {
		return true
	}
	if _, fresh := ast.Unparen(expr).(*ast.CompositeLit); !fresh && protectedAggregate(pass.TypesInfo.TypeOf(expr)) {
		return true
	}
	if protectedStateField(pass, resolver, expr, "workloadStopAttempt", mutationResultField) {
		return true
	}
	for _, field := range []string{methodStop, "phase"} {
		if protectedStateField(pass, resolver, expr, "workload", field) {
			return true
		}
	}
	for _, field := range []string{"uncertain", "terminal", mutationResultField, "persisted"} {
		if protectedStateField(pass, resolver, expr, "workloadStop", field) {
			return true
		}
	}
	return false
}

func protectedAggregate(t types.Type) bool {
	return isNamedPackageValueType(t, "workloadStop") || isNamedPackageValueType(t, "workload") || isNamedPackageValueType(t, "workloadStopAttempt")
}

func checkStateWrite(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, parents map[ast.Node]ast.Node, node ast.Node, lhs, rhs ast.Expr) {
	local := !enclosedByFuncLiteral(node, info.decl.Body, parents)
	checkMonotonicStateWrite(pass, info, resolver, node, lhs, rhs, local)
	checkTerminalStateWrite(pass, info, resolver, flow, node, lhs, rhs, local)
	checkPhaseStateWrite(pass, info, resolver, node, lhs, rhs, local)
	checkOwnerStateWrite(pass, info, resolver, flow, node, lhs, rhs, local)
	checkBindingWrite(pass, info, resolver, flow, node, lhs, rhs, local)
}
func checkMonotonicStateWrite(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, node ast.Node, lhs, rhs ast.Expr, local bool) {
	if protectedStateField(pass, resolver, lhs, "workloadStop", "persisted") {
		v, ok := proofValue(pass, info.decl.Body, resolver, rhs, nil)
		if !local || !isLocalMethod(info.fn, "dockerProvider", "persistOwnedStop") || !ok || v != 1 {
			pass.Reportf(node.Pos(), "[SLC106] durable persistence may only be recorded by persistOwnedStop")
		}
	}
	if protectedStateField(pass, resolver, lhs, "workloadStop", "uncertain") {
		v, ok := proofValue(pass, info.decl.Body, resolver, rhs, nil)
		if !ok || v != 1 {
			pass.Reportf(node.Pos(), "[SLC108] mutation uncertainty is monotonic and may only be set true")
		}
	}

}
func checkTerminalStateWrite(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, node ast.Node, lhs, rhs ast.Expr, local bool) {
	if protectedStateField(pass, resolver, lhs, "workloadStop", "terminal") || protectedStateField(pass, resolver, lhs, "workloadStop", mutationResultField) {
		canonicalOwner := terminalOwnerMatches(info, resolver, lhs)
		if !local || !terminalBoundary(info.fn) {
			pass.Reportf(node.Pos(), "[SLC108] terminal evidence may only be recorded by canonical attempt or observation boundaries")
		} else if !canonicalOwner {
			pass.Reportf(node.Pos(), "[SLC108] terminal evidence must belong to the canonical mutation owner")
		} else if isLocalMethod(info.fn, "workload", "applyStopAttempt") {
			checkTerminalEligibility(pass, info, resolver, flow, node)
		}
		checkTerminalResultWrite(pass, info, resolver, node, lhs, rhs)
		if protectedStateField(pass, resolver, lhs, "workloadStop", "terminal") {
			if v, ok := proofValue(pass, info.decl.Body, resolver, rhs, nil); !ok || v != 1 {
				pass.Reportf(node.Pos(), "[SLC108] terminal evidence may only become true")
			}
		}
	}

}

func terminalBoundary(fn *types.Func) bool {
	return isLocalMethod(fn, "workload", "applyStopAttempt") || isLocalMethod(fn, "dockerProvider", "recordObservedStopLocked")
}
func terminalOwnerMatches(info *functionInfo, resolver *pathResolver, lhs ast.Expr) bool {
	sig, _ := info.fn.Type().(*types.Signature)
	root, path, ok := resolver.resolveExpr(lhs)
	if !ok || sig == nil {
		return false
	}
	if isLocalMethod(info.fn, "workload", "applyStopAttempt") {
		return terminalOwnerPath(sig, 1, root, path, ".terminal", mutationResultPath)
	}
	if isLocalMethod(info.fn, "dockerProvider", "recordObservedStopLocked") {
		return terminalOwnerPath(sig, 0, root, path, ".stop.terminal", ".stop.result")
	}
	return false
}
func terminalOwnerPath(sig *types.Signature, index int, root *types.Var, path, terminal, result string) bool {
	return sig.Params().Len() > index && root == sig.Params().At(index) && (path == terminal || path == result)
}

func checkTerminalResultWrite(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, node ast.Node, lhs, rhs ast.Expr) {
	if protectedStateField(pass, resolver, lhs, "workloadStop", mutationResultField) {
		sig, _ := info.fn.Type().(*types.Signature)
		valid := false
		if isLocalMethod(info.fn, "workload", "applyStopAttempt") && sig.Params().Len() >= 3 {
			valid = sameResolvedValue(resolver, rhs, sig.Params().At(2), mutationResultPath)
		} else if isLocalMethod(info.fn, "dockerProvider", "recordObservedStopLocked") {
			value, known := proofValue(pass, info.decl.Body, resolver, rhs, nil)
			succeeded, exists := packageInt(pass, "workloadStopSucceeded")
			valid = known && exists && value == succeeded
		}
		if !valid {
			pass.Reportf(node.Pos(), "[SLC108] terminal result must preserve the canonical attempt or stopped evidence")
		}
	}

}
func checkPhaseStateWrite(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, node ast.Node, lhs, rhs ast.Expr, local bool) {
	if protectedStateField(pass, resolver, lhs, "workload", "phase") {
		sig, _ := info.fn.Type().(*types.Signature)
		root, path, ok := resolver.resolveExpr(lhs)
		valid := local && isLocalMethod(info.fn, "workload", "transitionLocked") && sig != nil && sig.Params().Len() == 1 && ok && root == sig.Recv() && path == ".phase" && sameResolvedValue(resolver, rhs, sig.Params().At(0), "")
		if !valid {
			pass.Reportf(node.Pos(), "[SLC109] workload phase writes must use the guarded transition boundary and preserve its requested phase")
		}
	}

}
func checkOwnerStateWrite(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, node ast.Node, lhs, rhs ast.Expr, local bool) {
	if protectedStateField(pass, resolver, lhs, "workloadStopAttempt", mutationResultField) {
		pass.Reportf(node.Pos(), "[SLC108] attempt result evidence may not be rewritten")
	}
	if protectedStateField(pass, resolver, lhs, "workload", methodStop) && !isNilValue(pass, info.decl.Body, rhs) {
		if !local || !isLocalMethod(info.fn, "workload", "newStopLocked") || !emptyStopProven(pass, info, resolver, flow, node) {
			pass.Reportf(node.Pos(), "[SLC107] installing a stop requires an empty owner at the canonical constructor")
		}
	}
	// Replacing the pointed-to operation discards uncertainty even if w.stop
	// still contains the same pointer.
	if protectedAggregate(pass.TypesInfo.TypeOf(lhs)) {
		if !freshIdentifier(pass, lhs) && !isNamedPackageValueType(pass.TypesInfo.TypeOf(lhs), "workload") {
			pass.Reportf(node.Pos(), "[SLC108] owned mutation values may not be replaced")
		}
	}
}

func emptyStopProven(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, node ast.Node) bool {
	sig, _ := info.fn.Type().(*types.Signature)
	return sig != nil && sig.Recv() != nil && !proofReaches(pass, info.decl.Body, resolver, flow, node, proofValues{{root: sig.Recv(), path: mutationStopPath}: 1})
}

func phaseGuardProven(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, node ast.Node) bool {
	sig := phaseGuardSignature(info, resolver, node)
	if sig == nil {
		return false
	}
	for _, name := range []string{"workloadDormant", "workloadStarting", "workloadReady", "workloadStopPending", "workloadFailed"} {
		v, ok := packageInt(pass, name)
		if !ok {
			return false
		}
		if proofReaches(pass, info.decl.Body, resolver, flow, node, proofValues{{root: sig.Recv(), path: mutationStopPath}: 1, {root: sig.Params().At(0)}: v}) {
			return false
		}
	}
	return true
}

func phaseGuardSignature(info *functionInfo, resolver *pathResolver, node ast.Node) *types.Signature {
	sig, _ := info.fn.Type().(*types.Signature)
	if sig == nil || sig.Recv() == nil || sig.Params().Len() != 1 {
		return nil
	}
	call, ok := node.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return nil
	}
	sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok || !sameResolvedValue(resolver, sel.X, sig.Recv(), "") || !sameResolvedValue(resolver, call.Args[0], sig.Params().At(0), "") {
		return nil
	}
	return sig
}

func checkTerminalEligibility(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, node ast.Node) {
	sig, _ := info.fn.Type().(*types.Signature)
	ambiguous, aok := packageInt(pass, "workloadStopAmbiguous")
	rejected, rok := packageInt(pass, "workloadStopRejected")
	if !aok || !rok || sig == nil || sig.Params().Len() < 3 {
		return
	}
	stop, attempt := sig.Params().At(1), sig.Params().At(2)
	for _, state := range [][2]int64{{ambiguous, 0}, {ambiguous, 1}, {rejected, 1}} {
		if proofReaches(pass, info.decl.Body, resolver, flow, node, proofValues{{root: stop, path: mutationUncertainPath}: state[1], {root: attempt, path: mutationResultPath}: state[0]}) {
			pass.Reportf(node.Pos(), "[SLC108] terminal settlement must exclude ambiguous attempts and rejection after uncertainty")
			return
		}
	}
}

func checkAmbiguousReturn(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow) {
	if !isLocalMethod(info.fn, "workload", "applyStopAttempt") {
		return
	}
	ambiguous, ok := packageInt(pass, "workloadStopAmbiguous")
	if !ok {
		return
	}
	sig, _ := info.fn.Type().(*types.Signature)
	if sig == nil || sig.Params().Len() < 3 {
		return
	}
	stop, attempt := sig.Params().At(1), sig.Params().At(2)
	values := proofValues{{root: stop, path: mutationUncertainPath}: 0, {root: attempt, path: mutationResultPath}: ambiguous}
	ast.Inspect(info.decl.Body, func(node ast.Node) bool {
		ret, ok := node.(*ast.ReturnStmt)
		if !ok || !proofReaches(pass, info.decl.Body, resolver, flow, ret, values) {
			return true
		}
		if guardedObsoleteReturn(pass, info, resolver, flow, ret, values) {
			return true
		}
		if !uncertaintyDominates(pass, info, resolver, flow, ret, stop) || !unsettledReturn(pass, info, resolver, ret) {
			pass.Reportf(ret.Pos(), "[SLC108] ambiguous attempt must record mutation uncertainty before returning unresolved")
		}
		return true
	})
}

func guardedObsoleteReturn(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, ret *ast.ReturnStmt, values proofValues) bool {
	return isObsoleteReturn(pass, info, resolver, ret) && obsoleteReturnGuarded(pass, info, resolver, flow, ret, values)
}
func isObsoleteReturn(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, ret *ast.ReturnStmt) bool {
	if len(ret.Results) != 1 {
		return false
	}
	obsolete, ok := packageInt(pass, "workloadStopObsolete")
	if !ok {
		return false
	}
	value := stableDefinitionExpr(pass, info.decl.Body, ret.Results[0], 0)
	v, known := proofValue(pass, info.decl.Body, resolver, value, nil)
	return known && v == obsolete
}
func uncertaintyDominates(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, ret *ast.ReturnStmt, stop *types.Var) bool {
	marked := false
	ast.Inspect(info.decl.Body, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok || !flow.dominates(assign, ret) {
			return true
		}
		marked = marked || assignmentMarksUncertainty(pass, info, resolver, assign, stop)
		return true
	})
	return marked
}
func assignmentMarksUncertainty(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, assign *ast.AssignStmt, stop *types.Var) bool {
	for i, lhs := range assign.Lhs {
		root, path, ok := resolver.resolveExpr(lhs)
		if !ok || root != stop || path != mutationUncertainPath || i >= len(assign.Rhs) {
			continue
		}
		if v, known := proofValue(pass, info.decl.Body, resolver, assign.Rhs[i], nil); known && v == 1 {
			return true
		}
	}
	return false
}

func unsettledReturn(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, ret *ast.ReturnStmt) bool {
	if len(ret.Results) != 1 {
		return false
	}
	value := stableDefinitionExpr(pass, info.decl.Body, ret.Results[0], 0)
	if unsettled, ok := packageInt(pass, "workloadStopUnsettled"); ok {
		if v, known := proofValue(pass, info.decl.Body, resolver, value, nil); known && v == unsettled {
			return true
		}
	}
	call, ok := ast.Unparen(value).(*ast.CallExpr)
	if !ok || !isLocalMethod(calledFunction(pass, call), "workload", "unsettleStopLocked") {
		return false
	}
	sel, _ := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	sig, _ := info.fn.Type().(*types.Signature)
	return sel != nil && sig != nil && sameResolvedValue(resolver, sel.X, sig.Recv(), "")
}

func obsoleteReturnGuarded(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, ret *ast.ReturnStmt, values proofValues) bool {
	guarded := false
	ast.Inspect(info.decl.Body, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 2 || len(assign.Rhs) != 1 {
			return true
		}
		call, ok := ast.Unparen(assign.Rhs[0]).(*ast.CallExpr)
		if !ok || !isLocalMethod(calledFunction(pass, call), "workload", "stopOwnershipLocked") {
			return true
		}
		id, ok := assign.Lhs[1].(*ast.Ident)
		if !ok {
			return true
		}
		owned, _ := pass.TypesInfo.Defs[id].(*types.Var)
		if owned == nil {
			return true
		}
		values[aliasTarget{root: owned}] = 1
		guarded = !proofReaches(pass, info.decl.Body, resolver, flow, ret, values)
		delete(values, aliasTarget{root: owned})
		return true
	})
	return guarded
}

func stoppedObservationProven(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, call *ast.CallExpr) bool {
	if !isLocalMethod(info.fn, "dockerProvider", "observeWorkloadLocked") {
		return false
	}
	sig, _ := info.fn.Type().(*types.Signature)
	return sig != nil && sig.Params().Len() == 2 && len(call.Args) == 1 && sameResolvedValue(resolver, call.Args[0], sig.Params().At(0), "") &&
		!proofReaches(pass, info.decl.Body, resolver, flow, call, proofValues{{root: sig.Params().At(1), path: mutationRunningPath}: 1})
}

func observedStopCallProven(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, call *ast.CallExpr) bool {
	sig, _ := info.fn.Type().(*types.Signature)
	if sig == nil || sig.Params().Len() == 0 || len(call.Args) != 1 {
		return false
	}
	w := sig.Params().At(0)
	if !sameResolvedValue(resolver, call.Args[0], w, "") {
		return false
	}
	if proofReaches(pass, info.decl.Body, resolver, flow, call, proofValues{{root: w, path: ".stop.issued"}: 1}) {
		return false
	}
	if isLocalMethod(info.fn, "dockerProvider", "observeStoppedWorkloadLocked") {
		return true
	}
	if !isLocalMethod(info.fn, "dockerProvider", "reconcileRetiredMutationObservationLocked") {
		return false
	}
	return retiredObservationLoopProven(pass, info, resolver, flow, call, sig)
}
func retiredObservationLoopProven(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, call *ast.CallExpr, sig *types.Signature) bool {
	proven := false
	ast.Inspect(info.decl.Body, func(node ast.Node) bool {
		loop, ok := node.(*ast.RangeStmt)
		if !ok {
			return true
		}
		container, start := observationLoopEntry(pass, resolver, flow, loop, call, sig)
		if container == nil {
			return true
		}
		if observationLoopExcludes(pass, info, resolver, flow, loop, call, start, sig.Params().At(0), container) {
			proven = true
		}
		return true
	})
	return proven
}
func observationLoopEntry(pass *analysis.Pass, resolver *pathResolver, flow *functionFlow, loop *ast.RangeStmt, call *ast.CallExpr, sig *types.Signature) (*types.Var, *cfg.Block) {
	if sig.Params().Len() < 2 || !sameResolvedValue(resolver, loop.X, sig.Params().At(1), "") {
		return nil, nil
	}
	if loop.Pos() > call.Pos() || !flow.dominates(loop.X, call) {
		return nil, nil
	}
	id, ok := loop.Value.(*ast.Ident)
	if !ok {
		return nil, nil
	}
	container, _ := pass.TypesInfo.Defs[id].(*types.Var)
	for _, block := range flow.graph.Blocks {
		if block.Kind == cfg.KindRangeBody && block.Stmt == loop {
			return container, block
		}
	}
	return nil, nil
}
func observationLoopExcludes(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, loop *ast.RangeStmt, call *ast.CallExpr, start *cfg.Block, w, container *types.Var) bool {
	if nodeContains(loop.Body, call) {
		other := proofValues{{root: w, path: mutationContainerIDPath}: 1, {root: container, path: ".ID"}: 2, {root: container, path: mutationRunningPath}: 0}
		if proofReachesFrom(pass, info.decl.Body, resolver, flow, start, call, other) {
			return false
		}
	}
	running := proofValues{{root: w, path: mutationContainerIDPath}: 1, {root: container, path: ".ID"}: 1, {root: container, path: mutationRunningPath}: 1}
	return !proofReachesFrom(pass, info.decl.Body, resolver, flow, start, call, running)
}

func attemptEvidenceProven(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, apply *ast.CallExpr) bool {
	sig := attemptApplySignature(info, resolver, apply)
	if sig == nil {
		return false
	}
	evidence := stableDefinitionExpr(pass, info.decl.Body, apply.Args[2], 0)
	if call, ok := ast.Unparen(evidence).(*ast.CallExpr); ok && isLocalMethod(calledFunction(pass, call), "dockerProvider", "attemptOwnedStop") {
		return matchingAttemptOwner(resolver, sig, call)
	}
	result := literalField(evidence, mutationResultField)
	if result == nil {
		return false
	}
	return terminalReplayProven(pass, info, resolver, flow, apply, sig, result)
}
func attemptApplySignature(info *functionInfo, resolver *pathResolver, apply *ast.CallExpr) *types.Signature {
	if !isLocalMethod(info.fn, "dockerProvider", "executeOwnedStopAttempt") || len(apply.Args) != 3 {
		return nil
	}
	sig, _ := info.fn.Type().(*types.Signature)
	if sig == nil || sig.Params().Len() < 3 {
		return nil
	}
	sel, _ := ast.Unparen(apply.Fun).(*ast.SelectorExpr)
	if sel == nil || !sameResolvedValue(resolver, sel.X, sig.Params().At(1), "") {
		return nil
	}
	if !sameResolvedValue(resolver, apply.Args[0], sig.Recv(), "") || !sameResolvedValue(resolver, apply.Args[1], sig.Params().At(2), "") {
		return nil
	}
	return sig
}
func matchingAttemptOwner(resolver *pathResolver, sig *types.Signature, call *ast.CallExpr) bool {
	sel, _ := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	return len(call.Args) == 3 && sel != nil && sameResolvedValue(resolver, sel.X, sig.Recv(), "") && sameResolvedValue(resolver, call.Args[1], sig.Params().At(1), "") && sameResolvedValue(resolver, call.Args[2], sig.Params().At(2), "")
}
func literalField(expr ast.Expr, name string) ast.Expr {
	literal, ok := ast.Unparen(expr).(*ast.CompositeLit)
	if !ok || len(literal.Elts) != 1 {
		return nil
	}
	field, ok := literal.Elts[0].(*ast.KeyValueExpr)
	if !ok {
		return nil
	}
	key, ok := field.Key.(*ast.Ident)
	if !ok || key.Name != name {
		return nil
	}
	return field.Value
}
func terminalReplayProven(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, apply *ast.CallExpr, sig *types.Signature, result ast.Expr) bool {
	valid := false
	ast.Inspect(info.decl.Body, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok || !flow.dominates(assign, apply) {
			return true
		}
		vars := replayAssignment(pass, resolver, sig, assign)
		if len(vars) != 3 || !sameResolvedValue(resolver, result, vars[0], "") {
			return true
		}
		valid = !proofReaches(pass, info.decl.Body, resolver, flow, apply, proofValues{{root: vars[1]}: 0}) && !proofReaches(pass, info.decl.Body, resolver, flow, apply, proofValues{{root: vars[2]}: 0})
		return true
	})
	return valid
}
func replayAssignment(pass *analysis.Pass, resolver *pathResolver, sig *types.Signature, assign *ast.AssignStmt) []*types.Var {
	if len(assign.Lhs) != 3 || len(assign.Rhs) != 1 {
		return nil
	}
	call, ok := ast.Unparen(assign.Rhs[0]).(*ast.CallExpr)
	if !ok || !isLocalMethod(calledFunction(pass, call), "workload", "stopResult") || len(call.Args) != 1 {
		return nil
	}
	sel, _ := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if sel == nil || !sameResolvedValue(resolver, sel.X, sig.Params().At(1), "") || !sameResolvedValue(resolver, call.Args[0], sig.Params().At(2), "") {
		return nil
	}
	return definedVariables(pass, assign.Lhs)
}
func definedVariables(pass *analysis.Pass, exprs []ast.Expr) []*types.Var {
	var vars []*types.Var
	for _, expr := range exprs {
		id, ok := expr.(*ast.Ident)
		if !ok {
			return nil
		}
		v, _ := pass.TypesInfo.Defs[id].(*types.Var)
		if v == nil {
			return nil
		}
		vars = append(vars, v)
	}
	return vars
}

func checkStateAddress(pass *analysis.Pass, resolver *pathResolver, n *ast.UnaryExpr) {
	if n.Op == token.AND && protectedStorage(pass, resolver, n.X) && !resolver.aliasRHS[n] {
		pass.Reportf(n.Pos(), "[SLC107] protected mutation state address may not escape its typed boundary")
	}
}
func synchronousEvidenceProven(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, parents map[ast.Node]ast.Node, n *ast.CallExpr) bool {
	return !enclosedByFuncLiteral(n, info.decl.Body, parents) && !insideDeferredOrGo(n, info.decl.Body, parents) && attemptEvidenceProven(pass, info, resolver, flow, n)
}
