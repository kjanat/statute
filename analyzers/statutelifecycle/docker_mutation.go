package statutelifecycle

import (
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/cfg"
)

const (
	statutePackagePath = "statute.kjanat.dev"
	dockerPackagePath  = statutePackagePath + "/internal/docker"
	dockerStartMethod  = "StartContainer"
	dockerStopMethod   = "StopContainer"
)

type dockerMutation uint8

const (
	dockerStart dockerMutation = iota
	dockerStop
)

func (m dockerMutation) method() string {
	if m == dockerStart {
		return dockerStartMethod
	}
	return dockerStopMethod
}

func (m dockerMutation) boundary() string {
	if m == dockerStart {
		return "(*dockerProvider).startActivation"
	}
	return "(*dockerProvider).attemptOwnedStop"
}

func checkDockerMutationInvariants(pass *analysis.Pass, functions map[*types.Func]*functionInfo, parents map[ast.Node]ast.Node) {
	if pass.Pkg == nil || !isStatutePackage(pass.Pkg.Path()) {
		return
	}
	for _, info := range functions {
		checkDockerMutationCalls(pass, info, parents)
		checkPersistBeforeStop(pass, info, parents)
		checkSettlementBoundaries(pass, info, parents)
	}
}

func isStatutePackage(path string) bool {
	return path == statutePackagePath || strings.HasPrefix(path, statutePackagePath+"/")
}

// SLC105 keeps raw Docker mutation capability inside the two workload
// boundaries and pins their context, client, and immutable binding shape.
func checkDockerMutationCalls(pass *analysis.Pass, info *functionInfo, parents map[ast.Node]ast.Node) {
	flow := newFunctionFlow(info.decl.Body)
	resolver := newPathResolver(pass, info.decl.Body)
	ast.Inspect(info.decl.Body, func(node ast.Node) bool {
		sel, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		mutation, ok := dockerMutationFor(selectedFunction(pass, sel))
		if !ok {
			mutation, ok = interfaceDockerMutation(pass, sel)
			if ok {
				pass.Reportf(sel.Pos(), "[SLC105] Docker %s interface dispatch cannot prove the canonical internal client boundary", mutation.method())
				return true
			}
		}
		if !ok {
			return true
		}
		call := directSelectorCall(sel, parents)
		if call == nil {
			pass.Reportf(sel.Pos(), "[SLC105] Docker %s reference escapes the canonical %s boundary", mutation.method(), mutation.boundary())
			return true
		}
		if !isMutationBoundary(info.fn, mutation) || enclosedByFuncLiteral(call, info.decl.Body, parents) {
			pass.Reportf(call.Pos(), "[SLC105] Docker %s may only be called directly from %s", mutation.method(), mutation.boundary())
			return true
		}
		if reason := validateDockerMutationCall(pass, info, call, mutation, resolver, flow, parents); reason != "" {
			pass.Reportf(call.Pos(), "[SLC105] Docker %s in %s %s", mutation.method(), mutation.boundary(), reason)
		}
		return true
	})
}

func dockerMutationFor(fn *types.Func) (dockerMutation, bool) {
	if !isMethod(fn, dockerPackagePath, "Client", dockerStartMethod) && !isMethod(fn, dockerPackagePath, "Client", dockerStopMethod) {
		return 0, false
	}
	if fn.Name() == dockerStartMethod {
		return dockerStart, true
	}
	return dockerStop, true
}

//nolint:gocyclo // Interface dispatch is rejected only after its full typed signature matches.
func interfaceDockerMutation(pass *analysis.Pass, sel *ast.SelectorExpr) (dockerMutation, bool) {
	selection := pass.TypesInfo.Selections[sel]
	fn := selectedFunction(pass, sel)
	if selection == nil || fn == nil || underlyingInterface(selection.Recv()) == nil {
		return 0, false
	}
	var mutation dockerMutation
	switch fn.Name() {
	case dockerStartMethod:
		mutation = dockerStart
	case dockerStopMethod:
		mutation = dockerStop
	default:
		return 0, false
	}
	sig, _ := fn.Type().(*types.Signature)
	errType := types.Universe.Lookup("error").Type()
	stringType := types.Universe.Lookup("string").Type()
	if sig == nil || sig.Params().Len() != 2 || sig.Results().Len() != 1 ||
		!isContextType(sig.Params().At(0).Type()) || !types.Identical(sig.Params().At(1).Type(), stringType) ||
		!types.Identical(sig.Results().At(0).Type(), errType) {
		return 0, false
	}
	return mutation, true
}

func underlyingInterface(t types.Type) *types.Interface {
	if t == nil {
		return nil
	}
	for {
		t = types.Unalias(t)
		pointer, ok := t.(*types.Pointer)
		if !ok {
			break
		}
		t = pointer.Elem()
	}
	iface, _ := t.Underlying().(*types.Interface)
	return iface
}

func isContextType(t types.Type) bool {
	named := namedType(t)
	return named != nil && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "context" && named.Obj().Name() == "Context"
}

func isMutationBoundary(fn *types.Func, mutation dockerMutation) bool {
	name := "startActivation"
	if mutation == dockerStop {
		name = "attemptOwnedStop"
	}
	return isLocalMethod(fn, "dockerProvider", name)
}

//nolint:gocyclo // Each rejected proof obligation needs its own diagnostic reason.
func validateDockerMutationCall(pass *analysis.Pass, info *functionInfo, call *ast.CallExpr, mutation dockerMutation, resolver *pathResolver, flow *functionFlow, parents map[ast.Node]ast.Node) string {
	if len(call.Args) != 2 {
		return "must pass one bounded context and one binding-aware target"
	}
	if insideDeferredOrGo(call, info.decl.Body, parents) {
		return "must execute synchronously inside its canonical boundary"
	}
	sig, _ := info.fn.Type().(*types.Signature)
	if sig == nil || sig.Recv() == nil || sig.Params().Len() < 3 {
		return "has an unexpected boundary signature"
	}
	sel, _ := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if sel == nil || !sameResolvedValue(resolver, sel.X, sig.Recv(), ".client") {
		return "must use the boundary provider's Docker client"
	}
	binding, ok := timeoutBindingFor(pass, info.decl.Body, call.Args[0])
	if !ok || !flow.dominates(binding.assignment, call) {
		return "must use a single-assignment context.WithTimeout result"
	}
	if !validMutationTimeout(pass, sig, binding.parent, binding.timeout, mutation, resolver) {
		return "must derive its timeout from the provider context and canonical deadline"
	}
	if !hasCancellationProof(pass, info.decl.Body, call, binding.cancel, flow, parents) {
		return "must cancel its bounded context on every normal return"
	}
	target := stableDefinitionExpr(pass, info.decl.Body, call.Args[1], 0)
	if !validMutationTarget(pass, sig, target, resolver) {
		return "must target workload.callRef for the operation's immutable binding"
	}
	return ""
}

type timeoutBinding struct {
	assignment ast.Node
	call       *ast.CallExpr
	cancel     *types.Var
	parent     ast.Expr
	timeout    ast.Expr
}

//nolint:gocyclo // Multi-result assignment validation is intentionally fail closed.
func timeoutBindingFor(pass *analysis.Pass, body *ast.BlockStmt, expr ast.Expr) (timeoutBinding, bool) {
	id, ok := ast.Unparen(expr).(*ast.Ident)
	if !ok {
		return timeoutBinding{}, false
	}
	ctxVar, _ := pass.TypesInfo.Uses[id].(*types.Var)
	if ctxVar == nil || collectMutatedVars(pass, body)[ctxVar] {
		return timeoutBinding{}, false
	}
	var found timeoutBinding
	definitions := 0
	ast.Inspect(body, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok || assign.Tok != token.DEFINE || len(assign.Lhs) != 2 || len(assign.Rhs) != 1 {
			return true
		}
		first, firstOK := assign.Lhs[0].(*ast.Ident)
		second, secondOK := assign.Lhs[1].(*ast.Ident)
		if !firstOK || !secondOK || pass.TypesInfo.Defs[first] != ctxVar {
			return true
		}
		definitions++
		cancel, _ := pass.TypesInfo.Defs[second].(*types.Var)
		call, _ := ast.Unparen(assign.Rhs[0]).(*ast.CallExpr)
		if cancel == nil || call == nil || len(call.Args) != 2 || !isPackageFunction(calledFunction(pass, call), "context", "WithTimeout") {
			return true
		}
		found = timeoutBinding{assignment: assign, call: call, cancel: cancel, parent: call.Args[0], timeout: call.Args[1]}
		return true
	})
	if definitions != 1 || found.call == nil || collectMutatedVars(pass, body)[found.cancel] {
		return timeoutBinding{}, false
	}
	return found, true
}

func validMutationTimeout(pass *analysis.Pass, sig *types.Signature, parent, timeout ast.Expr, mutation dockerMutation, resolver *pathResolver) bool {
	if !sameResolvedValue(resolver, parent, sig.Params().At(0), "") {
		return false
	}
	if mutation == dockerStart {
		return sameResolvedValue(resolver, timeout, sig.Params().At(2), ".policy.StartTimeout")
	}
	id, ok := ast.Unparen(timeout).(*ast.Ident)
	if !ok {
		return false
	}
	constant, _ := pass.TypesInfo.Uses[id].(*types.Const)
	return constant != nil && constant.Parent() == pass.Pkg.Scope() && constant.Name() == "workloadStopTimeout"
}

func validMutationTarget(pass *analysis.Pass, sig *types.Signature, expr ast.Expr, resolver *pathResolver) bool {
	call, ok := ast.Unparen(expr).(*ast.CallExpr)
	if !ok || len(call.Args) != 2 || !isLocalMethod(calledFunction(pass, call), "workload", "callRef") {
		return false
	}
	sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok || !sameResolvedValue(resolver, sel.X, sig.Params().At(1), "") {
		return false
	}
	owner := sig.Params().At(2)
	return sameResolvedValue(resolver, call.Args[0], owner, ".binding") && sameResolvedValue(resolver, call.Args[1], owner, ".ref")
}

func hasCancellationProof(pass *analysis.Pass, body *ast.BlockStmt, mutation *ast.CallExpr, cancel *types.Var, flow *functionFlow, parents map[ast.Node]ast.Node) bool {
	deferred := false
	ast.Inspect(body, func(node ast.Node) bool {
		stmt, ok := node.(*ast.DeferStmt)
		if !ok || enclosedByFuncLiteral(stmt, body, parents) || !callsVariable(pass, stmt.Call, cancel) {
			return true
		}
		if flow.dominates(stmt, mutation) {
			deferred = true
		}
		return true
	})
	if deferred {
		return true
	}
	return flow.allNormalReturnsAfter(mutation, func(call *ast.CallExpr) bool {
		return callsVariable(pass, call, cancel) && !insideDeferredOrGo(call, body, parents)
	})
}

func callsVariable(pass *analysis.Pass, call *ast.CallExpr, variable *types.Var) bool {
	if call == nil || len(call.Args) != 0 {
		return false
	}
	id, ok := ast.Unparen(call.Fun).(*ast.Ident)
	return ok && pass.TypesInfo.Uses[id] == variable
}

// SLC106 requires the nil edge of matching durable persistence to dominate
// every owned stop attempt.
func checkPersistBeforeStop(pass *analysis.Pass, info *functionInfo, parents map[ast.Node]ast.Node) {
	flow := newFunctionFlow(info.decl.Body)
	resolver := newPathResolver(pass, info.decl.Body)
	ast.Inspect(info.decl.Body, func(node ast.Node) bool {
		sel, ok := node.(*ast.SelectorExpr)
		if !ok || !isLocalMethod(selectedFunction(pass, sel), "dockerProvider", "attemptOwnedStop") {
			return true
		}
		call := directSelectorCall(sel, parents)
		if call == nil {
			pass.Reportf(sel.Pos(), "[SLC106] attemptOwnedStop must be called directly so persistence dominance remains provable")
			return true
		}
		if enclosedByFuncLiteral(call, info.decl.Body, parents) || !providerContextArgument(info.fn, call, resolver) || !persistenceGuardDominates(pass, info.decl.Body, call, resolver, flow, parents) {
			pass.Reportf(call.Pos(), "[SLC106] attemptOwnedStop must be dominated by successful persistOwnedStop for the same provider, workload, and stop")
		}
		return true
	})
}

func providerContextArgument(fn *types.Func, attempt *ast.CallExpr, resolver *pathResolver) bool {
	sig, _ := fn.Type().(*types.Signature)
	return sig != nil && sig.Params().Len() > 0 && len(attempt.Args) == 3 &&
		isContextType(sig.Params().At(0).Type()) && sameResolvedValue(resolver, attempt.Args[0], sig.Params().At(0), "")
}

//nolint:gocyclo // Persistence proof keeps every accepted syntax condition explicit.
func persistenceGuardDominates(pass *analysis.Pass, body *ast.BlockStmt, attempt *ast.CallExpr, resolver *pathResolver, flow *functionFlow, parents map[ast.Node]ast.Node) bool {
	attemptSel, ok := ast.Unparen(attempt.Fun).(*ast.SelectorExpr)
	if !ok || len(attempt.Args) != 3 {
		return false
	}
	matched := false
	ast.Inspect(body, func(node ast.Node) bool {
		if matched {
			return false
		}
		stmt, ok := node.(*ast.IfStmt)
		if !ok || enclosedByFuncLiteral(stmt, body, parents) || stmt.Init == nil || nodeContains(stmt.Body, attempt) || !blockEndsReturn(stmt.Body) || !flow.dominates(stmt.Cond, attempt) {
			return true
		}
		assign, ok := stmt.Init.(*ast.AssignStmt)
		if !ok || assign.Tok != token.DEFINE || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		errID, ok := assign.Lhs[0].(*ast.Ident)
		persist, okCall := ast.Unparen(assign.Rhs[0]).(*ast.CallExpr)
		if !ok || !okCall || len(persist.Args) != 2 || !isLocalMethod(calledFunction(pass, persist), "dockerProvider", "persistOwnedStop") {
			return true
		}
		errVar, _ := pass.TypesInfo.Defs[errID].(*types.Var)
		persistSel, _ := ast.Unparen(persist.Fun).(*ast.SelectorExpr)
		if errVar == nil || persistSel == nil || !conditionIsNonNil(pass, stmt.Cond, errVar) {
			return true
		}
		matched = sameExpressionValue(resolver, persistSel.X, attemptSel.X) &&
			sameExpressionValue(resolver, persist.Args[0], attempt.Args[1]) &&
			sameExpressionValue(resolver, persist.Args[1], attempt.Args[2])
		return true
	})
	return matched
}

// SLC107 confines durable deletion and in-memory release to settlement, then
// requires owner revalidation, generation fencing, and republication.
//
//nolint:gocyclo // One typed AST walk dispatches the complete settlement sink set.
func checkSettlementBoundaries(pass *analysis.Pass, info *functionInfo, parents map[ast.Node]ast.Node) {
	apply := isLocalMethod(info.fn, "workload", "applyStopAttempt")
	settle := isLocalMethod(info.fn, "workload", "settleStopLocked")
	supersede := isLocalMethod(info.fn, "workload", "supersedeBindingLocked")
	flow := newFunctionFlow(info.decl.Body)
	resolver := newPathResolver(pass, info.decl.Body)
	if apply && (directMethodCallCount(pass, info.decl.Body, "mutationRegistry", "delete", parents) != 1 ||
		directMethodCallCount(pass, info.decl.Body, "workload", "settleStopLocked", parents) != 1) {
		pass.Reportf(info.decl.Name.Pos(), "[SLC107] applyStopAttempt must contain exactly one canonical durable deletion and settlement call")
	}
	ast.Inspect(info.decl.Body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.CallExpr:
			fn := calledFunction(pass, n)
			nested := enclosedByFuncLiteral(n, info.decl.Body, parents)
			async := insideDeferredOrGo(n, info.decl.Body, parents)
			switch {
			case isLocalMethod(fn, "mutationRegistry", "delete") && (!apply || nested || async):
				pass.Reportf(n.Pos(), "[SLC107] durable mutation evidence may only be deleted by (*workload).applyStopAttempt")
			case isLocalMethod(fn, "workload", "settleStopLocked"):
				if !apply || nested || async {
					pass.Reportf(n.Pos(), "[SLC107] mutation ownership may only settle through (*workload).applyStopAttempt")
				} else if !canonicalSettlementProof(pass, info, n, resolver, flow, parents) {
					pass.Reportf(n.Pos(), "[SLC107] settlement must follow durable deletion, owner revalidation, generation fencing, and reconcile scheduling")
				}
			case closesStopDone(pass, info.decl.Body, resolver, n) && ((!settle && !supersede) || nested || async):
				pass.Reportf(n.Pos(), "[SLC107] mutation waiters may only be released by canonical settlement or binding supersession")
			case isLocalMethod(fn, "workload", "supersedeBindingLocked") && (nested || async || !supersessionGuarded(pass, info.decl.Body, n, resolver, parents)):
				pass.Reportf(n.Pos(), "[SLC107] mutation ownership may only be superseded after sameContainerLocked rejects the observed binding")
			}
		case *ast.AssignStmt:
			if clearsWorkloadStop(pass, info.decl.Body, resolver, n) && ((!settle && !supersede) || enclosedByFuncLiteral(n, info.decl.Body, parents)) {
				pass.Reportf(n.Pos(), "[SLC107] workload.stop may only be cleared by canonical settlement or binding supersession")
			}
		case *ast.SelectorExpr:
			if directSelectorCall(n, parents) != nil {
				break
			}
			fn := selectedFunction(pass, n)
			switch {
			case isLocalMethod(fn, "mutationRegistry", "delete"):
				pass.Reportf(n.Pos(), "[SLC107] durable mutation deletion must remain a direct canonical settlement call")
			case isLocalMethod(fn, "workload", "settleStopLocked"), isLocalMethod(fn, "workload", "supersedeBindingLocked"):
				pass.Reportf(n.Pos(), "[SLC107] mutation ownership release must remain a direct canonical call")
			}
		}
		return true
	})
}

//nolint:gocyclo // Settlement is accepted only when every independent proof holds.
func canonicalSettlementProof(pass *analysis.Pass, info *functionInfo, settlement *ast.CallExpr, resolver *pathResolver, flow *functionFlow, parents map[ast.Node]ast.Node) bool {
	sig, _ := info.fn.Type().(*types.Signature)
	settleSel, _ := ast.Unparen(settlement.Fun).(*ast.SelectorExpr)
	if sig == nil || sig.Recv() == nil || sig.Params().Len() < 2 || settleSel == nil || len(settlement.Args) != 3 {
		return false
	}
	w := sig.Recv()
	p := sig.Params().At(0)
	stop := sig.Params().At(1)
	if !sameResolvedValue(resolver, settleSel.X, w, "") || !sameResolvedValue(resolver, settlement.Args[0], p, "") || !sameResolvedValue(resolver, settlement.Args[1], stop, "") {
		return false
	}
	deletion, deleteOK := durableDeleteProven(pass, info.decl.Body, settlement, p, resolver, flow, parents)
	if !deleteOK {
		return false
	}
	if !ownershipCaptureProven(pass, info.decl.Body, settlement, deletion.owner, w, stop, resolver, flow, parents) {
		return false
	}
	ownerGuard := ownerRevalidation(pass, info.decl.Body, settlement, deletion.owner, w, deletion.call.End(), resolver, flow, parents)
	if ownerGuard == nil {
		return false
	}
	fenceAfter := max(ownerGuard.End(), deletion.guard.End())
	if !settlementFenced(pass, info.decl.Body, settlement, deletion.owner, w, p, fenceAfter, resolver, flow, parents) {
		return false
	}
	return flow.allNormalReturnsAfter(settlement, func(call *ast.CallExpr) bool {
		if !isLocalMethod(calledFunction(pass, call), "dockerProvider", "scheduleReconcile") {
			return false
		}
		sel, _ := ast.Unparen(call.Fun).(*ast.SelectorExpr)
		return sel != nil && sameResolvedValue(resolver, sel.X, p, "") && !insideDeferredOrGo(call, info.decl.Body, parents)
	})
}

//nolint:gocyclo // Capture provenance and its fail-closed owned guard are one proof.
func ownershipCaptureProven(pass *analysis.Pass, body *ast.BlockStmt, settlement *ast.CallExpr, owner, workload, stop *types.Var, resolver *pathResolver, flow *functionFlow, parents map[ast.Node]ast.Node) bool {
	var capture *ast.AssignStmt
	var owned *types.Var
	ast.Inspect(body, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok || enclosedByFuncLiteral(assign, body, parents) || assign.Tok != token.DEFINE || len(assign.Lhs) != 2 || len(assign.Rhs) != 1 {
			return true
		}
		ownerID, ownerOK := assign.Lhs[0].(*ast.Ident)
		ownedID, ownedOK := assign.Lhs[1].(*ast.Ident)
		call, callOK := ast.Unparen(assign.Rhs[0]).(*ast.CallExpr)
		if !ownerOK || !ownedOK || !callOK || pass.TypesInfo.Defs[ownerID] != owner ||
			!isLocalMethod(calledFunction(pass, call), "workload", "stopOwnershipLocked") || len(call.Args) != 1 {
			return true
		}
		sel, _ := ast.Unparen(call.Fun).(*ast.SelectorExpr)
		if sel == nil || !sameResolvedValue(resolver, sel.X, workload, "") || !sameResolvedValue(resolver, call.Args[0], stop, "") {
			return true
		}
		owned, _ = pass.TypesInfo.Defs[ownedID].(*types.Var)
		capture = assign
		return false
	})
	if capture == nil || owned == nil || !flow.dominates(capture, settlement) {
		return false
	}
	guarded := false
	ast.Inspect(body, func(node ast.Node) bool {
		stmt, ok := node.(*ast.IfStmt)
		if !ok || stmt.Pos() <= capture.End() || enclosedByFuncLiteral(stmt, body, parents) ||
			nodeContains(stmt.Body, settlement) || !conditionIsFalseVariable(pass, stmt.Cond, owned) ||
			!blockEndsReturn(stmt.Body) || !flow.dominates(stmt.Cond, settlement) {
			return true
		}
		guarded = true
		return false
	})
	return guarded
}

func directMethodCallCount(pass *analysis.Pass, body *ast.BlockStmt, receiver, method string, parents map[ast.Node]ast.Node) int {
	count := 0
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok && !enclosedByFuncLiteral(call, body, parents) && !insideDeferredOrGo(call, body, parents) &&
			isLocalMethod(calledFunction(pass, call), receiver, method) {
			count++
		}
		return true
	})
	return count
}

type durableDeleteProof struct {
	owner *types.Var
	call  *ast.CallExpr
	guard *ast.IfStmt
}

//nolint:gocyclo // Durable-delete provenance deliberately rejects ambiguous assignments.
func durableDeleteProven(pass *analysis.Pass, body *ast.BlockStmt, settlement *ast.CallExpr, provider *types.Var, resolver *pathResolver, flow *functionFlow, parents map[ast.Node]ast.Node) (durableDeleteProof, bool) {
	var deleteAssign *ast.AssignStmt
	var deleteCall *ast.CallExpr
	var persistErr *types.Var
	ast.Inspect(body, func(node ast.Node) bool {
		if deleteCall != nil {
			return false
		}
		assign, ok := node.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		call, ok := ast.Unparen(assign.Rhs[0]).(*ast.CallExpr)
		if !ok || enclosedByFuncLiteral(call, body, parents) || insideDeferredOrGo(call, body, parents) ||
			!isLocalMethod(calledFunction(pass, call), "mutationRegistry", "delete") || len(call.Args) != 1 {
			return true
		}
		id, ok := assign.Lhs[0].(*ast.Ident)
		if !ok {
			return true
		}
		v, _ := pass.TypesInfo.ObjectOf(id).(*types.Var)
		if v == nil {
			return true
		}
		deleteAssign, deleteCall, persistErr = assign, call, v
		return false
	})
	if deleteCall == nil {
		return durableDeleteProof{}, false
	}
	deleteSel, _ := ast.Unparen(deleteCall.Fun).(*ast.SelectorExpr)
	if deleteSel == nil || !registryFromProvider(pass, body, deleteSel.X, provider, resolver) {
		return durableDeleteProof{}, false
	}
	owner := selectedRootVar(resolver, deleteCall.Args[0], ".containerID")
	guard := errorSuccessGuard(pass, body, persistErr, settlement, deleteCall.End(), flow, parents)
	if owner == nil || guard == nil {
		return durableDeleteProof{}, false
	}
	if flow.dominates(deleteAssign, settlement) {
		return durableDeleteProof{owner: owner, call: deleteCall, guard: guard}, true
	}
	if !conditionalDeleteCoversAllPaths(pass, body, deleteAssign, deleteCall, persistErr, settlement, resolver, flow) {
		return durableDeleteProof{}, false
	}
	return durableDeleteProof{owner: owner, call: deleteCall, guard: guard}, true
}

func registryFromProvider(pass *analysis.Pass, body *ast.BlockStmt, registry ast.Expr, provider *types.Var, resolver *pathResolver) bool {
	source := stableDefinitionExpr(pass, body, registry, 0)
	call, ok := ast.Unparen(source).(*ast.CallExpr)
	if !ok || len(call.Args) != 0 || !isLocalMethod(calledFunction(pass, call), "dockerProvider", "currentMutationRegistry") {
		return false
	}
	sel, _ := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	return sel != nil && sameResolvedValue(resolver, sel.X, provider, "")
}

func conditionalDeleteCoversAllPaths(pass *analysis.Pass, body *ast.BlockStmt, deleteAssign *ast.AssignStmt, deleteCall *ast.CallExpr, persistErr *types.Var, settlement *ast.CallExpr, resolver *pathResolver, flow *functionFlow) bool {
	var enclosing *ast.IfStmt
	ast.Inspect(body, func(node ast.Node) bool {
		stmt, ok := node.(*ast.IfStmt)
		if ok && stmt.Else != nil && nodeContains(stmt.Else, deleteAssign) {
			enclosing = stmt
			return false
		}
		return true
	})
	if enclosing == nil || !flow.dominates(enclosing.Cond, settlement) {
		return false
	}
	registrySel, _ := ast.Unparen(deleteCall.Fun).(*ast.SelectorExpr)
	if registrySel == nil || !conditionIsNilValue(pass, enclosing.Cond, registrySel.X, resolver) {
		return false
	}
	if !blockAssignsNonNil(pass, enclosing.Body, persistErr) {
		return false
	}
	return assignmentsTo(pass, body, persistErr) == 2
}

func errorSuccessGuard(pass *analysis.Pass, body *ast.BlockStmt, errVar *types.Var, settlement *ast.CallExpr, after token.Pos, flow *functionFlow, parents map[ast.Node]ast.Node) *ast.IfStmt {
	var found *ast.IfStmt
	ast.Inspect(body, func(node ast.Node) bool {
		stmt, ok := node.(*ast.IfStmt)
		if !ok || stmt.Pos() <= after || enclosedByFuncLiteral(stmt, body, parents) ||
			!conditionIsNonNil(pass, stmt.Cond, errVar) || !blockEndsReturn(stmt.Body) {
			return true
		}
		if flow.dominates(stmt.Cond, settlement) {
			found = stmt
		}
		return true
	})
	return found
}

//nolint:gocyclo // Owner, order, exit, and provenance checks form one proof.
func ownerRevalidation(pass *analysis.Pass, body *ast.BlockStmt, settlement *ast.CallExpr, owner *types.Var, workload *types.Var, after token.Pos, resolver *pathResolver, flow *functionFlow, parents map[ast.Node]ast.Node) *ast.IfStmt {
	var found *ast.IfStmt
	ast.Inspect(body, func(node ast.Node) bool {
		stmt, ok := node.(*ast.IfStmt)
		if !ok || stmt.Pos() <= after || enclosedByFuncLiteral(stmt, body, parents) ||
			!blockEndsReturn(stmt.Body) || !flow.dominates(stmt.Cond, settlement) {
			return true
		}
		call := negatedCall(stmt.Cond)
		if call == nil || !isLocalMethod(calledFunction(pass, call), "workloadStopOwnership", "currentLocked") || len(call.Args) != 1 {
			return true
		}
		sel, _ := ast.Unparen(call.Fun).(*ast.SelectorExpr)
		if sel != nil && sameResolvedValue(resolver, sel.X, owner, "") && sameResolvedValue(resolver, call.Args[0], workload, "") {
			found = stmt
		}
		return true
	})
	return found
}

//nolint:gocyclo // Both typed generation fences must match exact settlement provenance.
func settlementFenced(pass *analysis.Pass, body *ast.BlockStmt, settlement *ast.CallExpr, owner, workload, provider *types.Var, after token.Pos, resolver *pathResolver, flow *functionFlow, parents map[ast.Node]ast.Node) bool {
	observations := false
	generation := false
	result := settlement.Args[2]
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || call.Pos() <= after || enclosedByFuncLiteral(call, body, parents) ||
			insideDeferredOrGo(call, body, parents) || !flow.dominates(call, settlement) {
			return true
		}
		sel, _ := ast.Unparen(call.Fun).(*ast.SelectorExpr)
		if sel == nil || !sameResolvedValue(resolver, sel.X, provider, "") {
			return true
		}
		switch {
		case isLocalMethod(calledFunction(pass, call), "dockerProvider", "invalidateWorkloadObservationsLocked") && len(call.Args) == 1:
			observations = sameResolvedValue(resolver, call.Args[0], workload, "")
		case isLocalMethod(calledFunction(pass, call), "dockerProvider", "invalidateStoppedGeneration") && len(call.Args) == 3:
			generation = sameResolvedValue(resolver, call.Args[0], owner, ".service") &&
				sameResolvedValue(resolver, call.Args[1], owner, ".bindingKey") &&
				sameExpressionValue(resolver, call.Args[2], result)
		}
		return true
	})
	return observations && generation
}

func supersessionGuarded(pass *analysis.Pass, body *ast.BlockStmt, supersede *ast.CallExpr, resolver *pathResolver, parents map[ast.Node]ast.Node) bool {
	supersedeSel, _ := ast.Unparen(supersede.Fun).(*ast.SelectorExpr)
	if supersedeSel == nil {
		return false
	}
	for node := parents[supersede]; node != nil && node != body; node = parents[node] {
		stmt, ok := node.(*ast.IfStmt)
		if !ok || !nodeContains(stmt.Body, supersede) {
			continue
		}
		call := negatedCall(stmt.Cond)
		if call == nil || !isLocalMethod(calledFunction(pass, call), "workload", "sameContainerLocked") {
			continue
		}
		sel, _ := ast.Unparen(call.Fun).(*ast.SelectorExpr)
		return sel != nil && sameExpressionValue(resolver, sel.X, supersedeSel.X)
	}
	return false
}

func closesStopDone(pass *analysis.Pass, body *ast.BlockStmt, resolver *pathResolver, call *ast.CallExpr) bool {
	id, ok := ast.Unparen(call.Fun).(*ast.Ident)
	builtin, _ := pass.TypesInfo.Uses[id].(*types.Builtin)
	if !ok || builtin == nil || builtin.Name() != "close" || len(call.Args) != 1 {
		return false
	}
	target := stableDefinitionExpr(pass, body, call.Args[0], 0)
	if isFieldSelection(pass, target, "workloadStop", "done") {
		return true
	}
	root, path, resolved := resolver.resolveExpr(target)
	return resolved && path == ".done" && isNamedPackageType(root.Type(), statutePackagePath, "workloadStop")
}

func clearsWorkloadStop(pass *analysis.Pass, body *ast.BlockStmt, resolver *pathResolver, assign *ast.AssignStmt) bool {
	for i, lhs := range assign.Lhs {
		root, path, ok := resolver.resolveExpr(lhs)
		if !ok || path != ".stop" || !isNamedPackageType(root.Type(), statutePackagePath, "workload") {
			continue
		}
		return i >= len(assign.Rhs) || isNilValue(pass, stableDefinitionExpr(pass, body, assign.Rhs[i], 0))
	}
	return false
}

func isFieldSelection(pass *analysis.Pass, expr ast.Expr, owner, field string) bool {
	selExpr, ok := ast.Unparen(expr).(*ast.SelectorExpr)
	if !ok {
		return false
	}
	sel := pass.TypesInfo.Selections[selExpr]
	fieldVar, _ := pass.TypesInfo.Uses[selExpr.Sel].(*types.Var)
	if sel == nil || sel.Kind() != types.FieldVal || fieldVar == nil || fieldVar.Name() != field {
		return false
	}
	named := namedType(sel.Recv())
	return named != nil && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == statutePackagePath && named.Obj().Name() == owner
}

func isNil(pass *analysis.Pass, expr ast.Expr) bool {
	id, ok := ast.Unparen(expr).(*ast.Ident)
	obj, _ := pass.TypesInfo.Uses[id].(*types.Nil)
	return ok && obj != nil
}

func isNilValue(pass *analysis.Pass, expr ast.Expr) bool {
	if isNil(pass, expr) {
		return true
	}
	call, ok := ast.Unparen(expr).(*ast.CallExpr)
	if !ok || len(call.Args) != 1 || !isNil(pass, call.Args[0]) {
		return false
	}
	tv, ok := pass.TypesInfo.Types[call.Fun]
	return ok && tv.IsType()
}

func isNamedPackageType(t types.Type, pkgPath, name string) bool {
	named := namedType(t)
	return named != nil && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == pkgPath && named.Obj().Name() == name
}

func isLocalMethod(fn *types.Func, receiver, method string) bool {
	return isMethod(fn, statutePackagePath, receiver, method)
}

func isMethod(fn *types.Func, pkgPath, receiver, method string) bool {
	if fn == nil || fn.Name() != method || fn.Pkg() == nil || fn.Pkg().Path() != pkgPath {
		return false
	}
	sig, _ := fn.Type().(*types.Signature)
	if sig == nil || sig.Recv() == nil {
		return false
	}
	named := namedType(sig.Recv().Type())
	return named != nil && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == pkgPath && named.Obj().Name() == receiver
}

func isPackageFunction(fn *types.Func, pkgPath, name string) bool {
	if fn == nil || fn.Name() != name || fn.Pkg() == nil || fn.Pkg().Path() != pkgPath {
		return false
	}
	sig, _ := fn.Type().(*types.Signature)
	return sig != nil && sig.Recv() == nil
}

func selectedFunction(pass *analysis.Pass, sel *ast.SelectorExpr) *types.Func {
	fn, _ := pass.TypesInfo.Uses[sel.Sel].(*types.Func)
	return fn
}

func directSelectorCall(sel *ast.SelectorExpr, parents map[ast.Node]ast.Node) *ast.CallExpr {
	var node ast.Node = sel
	for {
		parent := parents[node]
		if paren, ok := parent.(*ast.ParenExpr); ok && paren.X == node {
			node = paren
			continue
		}
		call, ok := parent.(*ast.CallExpr)
		if ok && call.Fun == node {
			return call
		}
		return nil
	}
}

func enclosedByFuncLiteral(node ast.Node, body *ast.BlockStmt, parents map[ast.Node]ast.Node) bool {
	for current := parents[node]; current != nil && current != body; current = parents[current] {
		if _, ok := current.(*ast.FuncLit); ok {
			return true
		}
	}
	return false
}

func insideDeferredOrGo(node ast.Node, body *ast.BlockStmt, parents map[ast.Node]ast.Node) bool {
	for current := parents[node]; current != nil && current != body; current = parents[current] {
		switch current.(type) {
		case *ast.DeferStmt, *ast.GoStmt:
			return true
		}
	}
	return false
}

func sameResolvedValue(resolver *pathResolver, expr ast.Expr, root *types.Var, path string) bool {
	if expr == nil || root == nil {
		return false
	}
	gotRoot, gotPath, ok := resolver.resolve(expr)
	return ok && gotRoot == root && gotPath == path
}

func sameExpressionValue(resolver *pathResolver, left, right ast.Expr) bool {
	leftRoot, leftPath, leftOK := resolver.resolve(left)
	rightRoot, rightPath, rightOK := resolver.resolve(right)
	return leftOK && rightOK && leftRoot == rightRoot && leftPath == rightPath
}

func selectedRootVar(resolver *pathResolver, expr ast.Expr, path string) *types.Var {
	root, gotPath, ok := resolver.resolve(expr)
	if !ok || gotPath != path {
		return nil
	}
	return root
}

func stableDefinitionExpr(pass *analysis.Pass, body *ast.BlockStmt, expr ast.Expr, depth int) ast.Expr {
	if depth > 8 {
		return expr
	}
	id, ok := ast.Unparen(expr).(*ast.Ident)
	if !ok {
		return expr
	}
	v, _ := pass.TypesInfo.Uses[id].(*types.Var)
	if v == nil || collectMutatedVars(pass, body)[v] {
		return expr
	}
	definition := definitionExpr(pass, body, v)
	if definition == nil {
		return expr
	}
	return stableDefinitionExpr(pass, body, definition, depth+1)
}

func definitionExpr(pass *analysis.Pass, body *ast.BlockStmt, variable *types.Var) ast.Expr {
	var out ast.Expr
	count := 0
	ast.Inspect(body, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok || assign.Tok != token.DEFINE || len(assign.Lhs) != len(assign.Rhs) {
			return true
		}
		for i, lhs := range assign.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok || pass.TypesInfo.Defs[id] != variable {
				continue
			}
			count++
			out = assign.Rhs[i]
		}
		return true
	})
	if count != 1 {
		return nil
	}
	return out
}

func conditionIsNonNil(pass *analysis.Pass, expr ast.Expr, variable *types.Var) bool {
	binary, ok := ast.Unparen(expr).(*ast.BinaryExpr)
	if !ok || binary.Op != token.NEQ {
		return false
	}
	return (identIs(pass, binary.X, variable) && isNil(pass, binary.Y)) ||
		(isNil(pass, binary.X) && identIs(pass, binary.Y, variable))
}

func conditionIsFalseVariable(pass *analysis.Pass, expr ast.Expr, variable *types.Var) bool {
	unary, ok := ast.Unparen(expr).(*ast.UnaryExpr)
	return ok && unary.Op == token.NOT && identIs(pass, unary.X, variable)
}

func conditionIsNilValue(pass *analysis.Pass, expr, value ast.Expr, resolver *pathResolver) bool {
	binary, ok := ast.Unparen(expr).(*ast.BinaryExpr)
	if !ok || binary.Op != token.EQL {
		return false
	}
	return (sameExpressionValue(resolver, binary.X, value) && isNil(pass, binary.Y)) ||
		(isNil(pass, binary.X) && sameExpressionValue(resolver, binary.Y, value))
}

func identIs(pass *analysis.Pass, expr ast.Expr, variable *types.Var) bool {
	id, ok := ast.Unparen(expr).(*ast.Ident)
	return ok && pass.TypesInfo.Uses[id] == variable
}

func negatedCall(expr ast.Expr) *ast.CallExpr {
	unary, ok := ast.Unparen(expr).(*ast.UnaryExpr)
	if !ok || unary.Op != token.NOT {
		return nil
	}
	call, _ := ast.Unparen(unary.X).(*ast.CallExpr)
	return call
}

func blockEndsReturn(block *ast.BlockStmt) bool {
	if block == nil || len(block.List) == 0 {
		return false
	}
	_, ok := block.List[len(block.List)-1].(*ast.ReturnStmt)
	return ok
}

func blockAssignsNonNil(pass *analysis.Pass, block *ast.BlockStmt, variable *types.Var) bool {
	for _, stmt := range block.List {
		assign, ok := stmt.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			continue
		}
		id, ok := assign.Lhs[0].(*ast.Ident)
		call, callOK := ast.Unparen(assign.Rhs[0]).(*ast.CallExpr)
		if ok && pass.TypesInfo.Uses[id] == variable && callOK && isPackageFunction(calledFunction(pass, call), "errors", "New") {
			return true
		}
	}
	return false
}

func assignmentsTo(pass *analysis.Pass, body *ast.BlockStmt, variable *types.Var) int {
	count := 0
	ast.Inspect(body, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, lhs := range assign.Lhs {
			id, ok := lhs.(*ast.Ident)
			if ok && (pass.TypesInfo.Uses[id] == variable || pass.TypesInfo.Defs[id] == variable) {
				count++
			}
		}
		return true
	})
	return count
}

func nodeContains(root, target ast.Node) bool {
	return root != nil && target != nil && root.Pos() <= target.Pos() && target.End() <= root.End()
}

type flowPoint struct {
	block *cfg.Block
	index int
	node  ast.Node
}

type functionFlow struct {
	graph      *cfg.CFG
	dominators map[*cfg.Block]map[*cfg.Block]bool
}

func newFunctionFlow(body *ast.BlockStmt) *functionFlow {
	graph := cfg.New(body, assumeCallReturns)
	return &functionFlow{graph: graph, dominators: blockDominators(graph)}
}

func (f *functionFlow) dominates(before, after ast.Node) bool {
	first, firstOK := f.point(before)
	second, secondOK := f.point(after)
	if !firstOK || !secondOK {
		return false
	}
	if first.block == second.block {
		return first.index <= second.index
	}
	return f.dominators[second.block][first.block]
}

func (f *functionFlow) point(target ast.Node) (flowPoint, bool) {
	var best flowPoint
	bestSpan := int(^uint(0) >> 1)
	for _, block := range f.graph.Blocks {
		if !block.Live {
			continue
		}
		for i, node := range block.Nodes {
			if !nodeContains(node, target) {
				continue
			}
			span := int(node.End() - node.Pos())
			if span < bestSpan {
				best = flowPoint{block: block, index: i, node: node}
				bestSpan = span
			}
		}
	}
	return best, best.block != nil
}

//nolint:gocyclo // The finite CFG walk tracks both path position and proof state.
func (f *functionFlow) allNormalReturnsAfter(start ast.Node, predicate func(*ast.CallExpr) bool) bool {
	point, ok := f.point(start)
	if !ok {
		return false
	}
	if _, returning := point.node.(*ast.ReturnStmt); returning {
		return false
	}
	type state struct {
		block *cfg.Block
		index int
		seen  bool
	}
	queue := []state{{block: point.block, index: point.index + 1}}
	visited := make(map[state]bool)
	reachedReturn := false
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if current.block == nil || !current.block.Live || visited[current] {
			continue
		}
		visited[current] = true
		seen := current.seen
		for _, node := range current.block.Nodes[current.index:] {
			if nodeHasCall(node, predicate) {
				seen = true
			}
			if _, returning := node.(*ast.ReturnStmt); returning {
				reachedReturn = true
				if !seen {
					return false
				}
			}
		}
		for _, succ := range current.block.Succs {
			queue = append(queue, state{block: succ, seen: seen})
		}
	}
	return reachedReturn
}

func nodeHasCall(node ast.Node, predicate func(*ast.CallExpr) bool) bool {
	found := false
	ast.Inspect(node, func(current ast.Node) bool {
		if current == nil || found {
			return false
		}
		if _, literal := current.(*ast.FuncLit); literal {
			return false
		}
		if call, ok := current.(*ast.CallExpr); ok && predicate(call) {
			found = true
			return false
		}
		return true
	})
	return found
}

//nolint:gocyclo // Standard iterative dominator construction is clearest in one loop.
func blockDominators(graph *cfg.CFG) map[*cfg.Block]map[*cfg.Block]bool {
	live := make([]*cfg.Block, 0, len(graph.Blocks))
	pred := make(map[*cfg.Block][]*cfg.Block)
	for _, block := range graph.Blocks {
		if !block.Live {
			continue
		}
		live = append(live, block)
		for _, succ := range block.Succs {
			if succ.Live {
				pred[succ] = append(pred[succ], block)
			}
		}
	}
	dom := make(map[*cfg.Block]map[*cfg.Block]bool, len(live))
	for _, block := range live {
		dom[block] = make(map[*cfg.Block]bool, len(live))
		if block == graph.Blocks[0] {
			dom[block][block] = true
			continue
		}
		for _, candidate := range live {
			dom[block][candidate] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for _, block := range live {
			if block == graph.Blocks[0] {
				continue
			}
			next := make(map[*cfg.Block]bool, len(live))
			for _, candidate := range live {
				included := len(pred[block]) > 0
				for _, predecessor := range pred[block] {
					included = included && dom[predecessor][candidate]
				}
				if included {
					next[candidate] = true
				}
			}
			next[block] = true
			if !sameBlockSet(dom[block], next) {
				dom[block] = next
				changed = true
			}
		}
	}
	return dom
}

func sameBlockSet(left, right map[*cfg.Block]bool) bool {
	if len(left) != len(right) {
		return false
	}
	for block := range left {
		if !right[block] {
			return false
		}
	}
	return true
}
