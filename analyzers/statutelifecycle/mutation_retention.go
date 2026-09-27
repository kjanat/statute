package statutelifecycle

import (
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/cfg"
)

const retiredRegistryField = "retiredMutations"
const workloadRegistryField = "workloadEntries"

type registryReference struct {
	provider *types.Var
	field    string
	index    ast.Expr
	sliced   bool
}

func registryReferenceFor(pass *analysis.Pass, body *ast.BlockStmt, resolver *pathResolver, expr ast.Expr, depth int) (registryReference, bool) {
	if expr == nil || depth > 12 {
		return registryReference{}, false
	}
	switch n := ast.Unparen(expr).(type) {
	case *ast.SelectorExpr:
		return registryFieldReference(pass, resolver, n)
	case *ast.CallExpr:
		return registryAppendReference(pass, body, resolver, n, depth)
	case *ast.Ident:
		return registryIdentifierReference(pass, body, resolver, n, depth)
	case *ast.IndexExpr:
		r, ok := registryReferenceFor(pass, body, resolver, n.X, depth+1)
		r.index = n.Index
		return r, ok
	case *ast.SliceExpr:
		r, ok := registryReferenceFor(pass, body, resolver, n.X, depth+1)
		r.sliced = true
		return r, ok
	case *ast.TypeAssertExpr:
		return registryReferenceFor(pass, body, resolver, n.X, depth+1)
	}
	return registryAddressReference(pass, body, resolver, expr, depth)
}

func registryAddressReference(pass *analysis.Pass, body *ast.BlockStmt, resolver *pathResolver, expr ast.Expr, depth int) (registryReference, bool) {
	switch n := ast.Unparen(expr).(type) {
	case *ast.UnaryExpr:
		if n.Op == token.AND {
			return registryReferenceFor(pass, body, resolver, n.X, depth+1)
		}
	case *ast.StarExpr:
		return registryReferenceFor(pass, body, resolver, n.X, depth+1)
	}
	return registryReference{}, false
}

func registryAppendReference(pass *analysis.Pass, body *ast.BlockStmt, resolver *pathResolver, call *ast.CallExpr, depth int) (registryReference, bool) {
	if builtinCall(pass, call, "append") && len(call.Args) > 0 {
		return registryReferenceFor(pass, body, resolver, call.Args[0], depth+1)
	}
	return registryReference{}, false
}

func registryIdentifierReference(pass *analysis.Pass, body *ast.BlockStmt, resolver *pathResolver, id *ast.Ident, depth int) (registryReference, bool) {
	v, _ := pass.TypesInfo.Uses[id].(*types.Var)
	if v == nil {
		v, _ = pass.TypesInfo.Defs[id].(*types.Var)
	}
	if v == nil {
		return registryReference{}, false
	}
	return registryReferenceFor(pass, body, resolver, definitionExpr(pass, body, v), depth+1)
}

func registryFieldReference(pass *analysis.Pass, resolver *pathResolver, sel *ast.SelectorExpr) (registryReference, bool) {
	for _, field := range []string{retiredRegistryField, workloadRegistryField} {
		if isFieldSelection(pass, sel, "dockerProvider", field) {
			root, path, ok := resolver.resolveExpr(sel.X)
			if !ok || path != "" {
				return registryReference{field: field}, true
			}
			return registryReference{provider: root, field: field}, true
		}
	}
	return registryReference{}, false
}

func checkMutationRetention(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, parents map[ast.Node]ast.Node) {
	ast.Inspect(info.decl.Body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.CompositeLit:
			checkRegistryComposite(pass, info, resolver, n)
		case *ast.SendStmt:
			checkRegistrySend(pass, info, resolver, n)
		case *ast.AssignStmt:
			checkRegistryAssignment(pass, info, resolver, flow, parents, n)
		case *ast.RangeStmt:
			checkRegistryRange(pass, info, resolver, n)
		case *ast.CallExpr:
			checkRegistryCall(pass, info, resolver, parents, n)
		case *ast.ReturnStmt:
			checkRegistryReturn(pass, info, resolver, n)
		case *ast.UnaryExpr:
			if n.Op == token.AND {
				if _, ok := registryReferenceFor(pass, info.decl.Body, resolver, n.X, 0); ok {
					pass.Reportf(n.Pos(), "[SLC107] mutation owner registry address may not escape")
				}
			}
		}
		return true
	})
}

func checkRegistrySend(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, send *ast.SendStmt) {
	if source, ok := registryReferenceFor(pass, info.decl.Body, resolver, send.Value, 0); ok && source.index == nil {
		pass.Reportf(send.Pos(), "[SLC107] mutation owner registry may not escape through a channel")
	}
}

func checkRegistryRange(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, stmt *ast.RangeStmt) {
	if stmt.Tok != token.ASSIGN {
		return
	}
	for _, target := range []ast.Expr{stmt.Key, stmt.Value} {
		if target != nil && isNamedPackageValueType(pass.TypesInfo.TypeOf(target), "dockerProvider") {
			pass.Reportf(target.Pos(), "[SLC107] provider values may not be replaced by range assignment")
			continue
		}
		if _, ok := registryReferenceFor(pass, info.decl.Body, resolver, target, 0); ok {
			pass.Reportf(target.Pos(), "[SLC107] mutation owner registry may not be overwritten by range assignment")
		}
	}
}

func checkRegistryReturn(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, ret *ast.ReturnStmt) {
	for _, result := range ret.Results {
		if r, ok := registryReferenceFor(pass, info.decl.Body, resolver, result, 0); ok && r.index == nil {
			pass.Reportf(result.Pos(), "[SLC107] mutation owner registry may not escape through a return")
		}
	}
}

func checkRegistryComposite(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, literal *ast.CompositeLit) {
	for _, element := range literal.Elts {
		value := element
		if kv, ok := element.(*ast.KeyValueExpr); ok {
			value = kv.Value
		}
		if source, ok := registryReferenceFor(pass, info.decl.Body, resolver, value, 0); ok && source.index == nil {
			pass.Reportf(value.Pos(), "[SLC107] mutation owner registry may not escape through composite storage")
		}
	}
}

func checkRegistryAssignment(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, parents map[ast.Node]ast.Node, assign *ast.AssignStmt) {
	for i, lhs := range assign.Lhs {
		if freshIdentifier(pass, lhs) {
			continue
		}
		if isNamedPackageValueType(pass.TypesInfo.TypeOf(lhs), "dockerProvider") {
			pass.Reportf(lhs.Pos(), "[SLC107] provider values may not be replaced because they own mutation registries")
			continue
		}
		r, ok := registryReferenceFor(pass, info.decl.Body, resolver, lhs, 0)
		if localRegistryValue(lhs, r, ok) {
			continue
		}
		if !ok {
			checkRegistryAssignmentEscape(pass, info, resolver, assign, i)
			continue
		}
		var rhs ast.Expr
		if i < len(assign.Rhs) {
			rhs = assign.Rhs[i]
		}
		if enclosedByFuncLiteral(assign, info.decl.Body, parents) || !registryAssignmentProven(pass, info, resolver, flow, assign, r, rhs) {
			pass.Reportf(lhs.Pos(), "[SLC107] mutation owner registry changes must preserve every outstanding owner")
		}
	}
}

func localRegistryValue(lhs ast.Expr, r registryReference, known bool) bool {
	_, local := ast.Unparen(lhs).(*ast.Ident)
	return local && known && r.index != nil
}

func checkRegistryAssignmentEscape(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, assign *ast.AssignStmt, index int) {
	if index >= len(assign.Rhs) {
		return
	}
	if source, known := registryReferenceFor(pass, info.decl.Body, resolver, assign.Rhs[index], 0); known && source.index == nil {
		pass.Reportf(assign.Lhs[index].Pos(), "[SLC107] mutation owner registry may not escape through assignment")
	}
}

func freshIdentifier(pass *analysis.Pass, expr ast.Expr) bool {
	id, ok := ast.Unparen(expr).(*ast.Ident)
	return ok && pass.TypesInfo.Defs[id] != nil
}

func registryAssignmentProven(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, assign *ast.AssignStmt, r registryReference, rhs ast.Expr) bool {
	if r.provider == nil {
		return false
	}
	if r.field == workloadRegistryField {
		return workloadRegistryAssignment(pass, info, resolver, flow, assign, r, rhs)
	}
	if r.index != nil {
		return false
	}
	if preservingRegistryAppend(pass, info, resolver, r, rhs) {
		return true
	}
	if !isLocalMethod(info.fn, "dockerProvider", "retiredMutationContainerRefsLocked") {
		return false
	}
	if r.sliced {
		return pruneAppendOwner(pass, info, resolver, r, rhs) != nil
	}
	return registryPruneProven(pass, info, resolver, flow, assign, r, rhs)
}

func preservingRegistryAppend(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, r registryReference, rhs ast.Expr) bool {
	call, ok := ast.Unparen(rhs).(*ast.CallExpr)
	if !ok || !builtinCall(pass, call, "append") || len(call.Args) < 2 {
		return false
	}
	base, ok := registryReferenceFor(pass, info.decl.Body, resolver, call.Args[0], 0)
	return ok && sameRegistry(r, base) && !r.sliced && !base.sliced && base.index == nil
}

func sameRegistry(a, b registryReference) bool { return a.provider == b.provider && a.field == b.field }

func builtinCall(pass *analysis.Pass, call *ast.CallExpr, name string) bool {
	id, ok := ast.Unparen(call.Fun).(*ast.Ident)
	if !ok {
		return false
	}
	builtin, _ := pass.TypesInfo.Uses[id].(*types.Builtin)
	return builtin != nil && builtin.Name() == name
}

func checkRegistryCall(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, parents map[ast.Node]ast.Node, call *ast.CallExpr) {
	for i, arg := range call.Args {
		r, ok := registryReferenceFor(pass, info.decl.Body, resolver, arg, 0)
		if !ok || r.index != nil {
			continue
		}
		if builtinCall(pass, call, "len") {
			continue
		}
		if builtinCall(pass, call, "append") && i == 0 && (!r.sliced || pruneAppendCallProven(pass, info, resolver, parents, call, r)) {
			continue
		}
		pass.Reportf(arg.Pos(), "[SLC107] mutation owner registry may not be cleared, deleted, or passed to an unproved helper")
	}
}

func pruneAppendCallProven(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, parents map[ast.Node]ast.Node, call *ast.CallExpr, r registryReference) bool {
	if !isLocalMethod(info.fn, "dockerProvider", "retiredMutationContainerRefsLocked") || len(call.Args) != 2 {
		return false
	}
	assign, ok := parents[call].(*ast.AssignStmt)
	if !ok || len(assign.Lhs) != 1 {
		return false
	}
	if !sameAssignedVariable(pass, assign.Lhs[0], call.Args[0]) {
		return false
	}
	for node := parents[call]; node != nil && node != info.decl.Body; node = parents[node] {
		if loop, ok := node.(*ast.RangeStmt); ok {
			return pruneAppendLoopOwner(pass, info, resolver, loop, call, r)
		}
		if _, ok := node.(*ast.ForStmt); ok {
			return false
		}
	}
	return false
}

func sameAssignedVariable(pass *analysis.Pass, left, right ast.Expr) bool {
	a, aok := ast.Unparen(left).(*ast.Ident)
	b, bok := ast.Unparen(right).(*ast.Ident)
	return aok && bok && pass.TypesInfo.Uses[a] != nil && pass.TypesInfo.Uses[a] == pass.TypesInfo.Uses[b]
}

func pruneAppendLoopOwner(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, loop *ast.RangeStmt, call *ast.CallExpr, r registryReference) bool {
	base, ok := registryReferenceFor(pass, info.decl.Body, resolver, loop.X, 0)
	if !ok || !sameRegistry(r, base) || base.sliced {
		return false
	}
	id, ok := loop.Value.(*ast.Ident)
	if !ok {
		return false
	}
	owner, _ := pass.TypesInfo.Defs[id].(*types.Var)
	return owner != nil && sameResolvedValue(resolver, call.Args[1], owner, "")
}

func workloadRegistryAssignment(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, assign *ast.AssignStmt, r registryReference, rhs ast.Expr) bool {
	if r.index == nil {
		return emptyRegistryInitialization(pass, info, resolver, flow, assign, r, rhs)
	}
	if isLocalMethod(info.fn, "dockerProvider", "prepareWorkloadObservationLocked") {
		return registryEntryGuarded(pass, info, resolver, flow, assign, r, nil)
	}
	if !isLocalMethod(info.fn, "dockerProvider", "detachMutationOwnerHeldLocked") {
		return false
	}
	sig, _ := info.fn.Type().(*types.Signature)
	if sig == nil || sig.Params().Len() != 2 {
		return false
	}
	old := sig.Params().At(0)
	if !registryEntryGuarded(pass, info, resolver, flow, assign, r, old) {
		return false
	}
	return retiredOwnerPreserved(pass, info, resolver, flow, assign, r.provider, old)
}

func emptyRegistryInitialization(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, assign *ast.AssignStmt, r registryReference, rhs ast.Expr) bool {
	literal, ok := ast.Unparen(rhs).(*ast.CompositeLit)
	if !ok || len(literal.Elts) != 0 {
		return false
	}
	guarded := false
	ast.Inspect(info.decl.Body, func(node ast.Node) bool {
		stmt, ok := node.(*ast.IfStmt)
		if !ok || !flow.dominates(stmt.Cond, assign) {
			return true
		}
		if nilRegistryCondition(pass, info, resolver, stmt.Cond, r) && flow.falseBranchExcludes(stmt.Cond, assign) {
			guarded = true
		}
		return true
	})
	return guarded
}
func nilRegistryCondition(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, condition ast.Expr, r registryReference) bool {
	binary, ok := ast.Unparen(condition).(*ast.BinaryExpr)
	if !ok || binary.Op != token.EQL || !isNil(pass, binary.Y) {
		return false
	}
	guard, ok := registryReferenceFor(pass, info.decl.Body, resolver, binary.X, 0)
	return ok && sameRegistry(r, guard) && guard.index == nil
}

func registryEntryGuarded(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, target ast.Node, r registryReference, owner *types.Var) bool {
	guarded := false
	ast.Inspect(info.decl.Body, func(node ast.Node) bool {
		stmt, ok := node.(*ast.IfStmt)
		if !ok || !flow.dominates(stmt.Cond, target) {
			return true
		}
		if registryEntryCondition(pass, info, resolver, stmt.Cond, r, owner) && branchPanics(pass, stmt.Body) {
			guarded = true
		}
		return true
	})
	return guarded
}
func registryEntryCondition(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, condition ast.Expr, r registryReference, owner *types.Var) bool {
	binary, ok := ast.Unparen(condition).(*ast.BinaryExpr)
	if !ok || binary.Op != token.NEQ {
		return false
	}
	guard, ok := registryReferenceFor(pass, info.decl.Body, resolver, binary.X, 0)
	if !ok || !sameRegistry(r, guard) || !sameExpressionValue(resolver, r.index, guard.index) {
		return false
	}
	if owner != nil {
		return sameResolvedValue(resolver, binary.Y, owner, "")
	}
	return isNil(pass, binary.Y)
}

func branchPanics(pass *analysis.Pass, body *ast.BlockStmt) bool {
	flow := newFunctionFlow(body)
	queue := []*cfg.Block{flow.graph.Blocks[0]}
	seen := make(map[*cfg.Block]bool)
	panicked := false
	for len(queue) > 0 {
		block := queue[0]
		queue = queue[1:]
		if seen[block] || !block.Live {
			continue
		}
		seen[block] = true
		if registryBlockPanics(pass, block) {
			panicked = true
			continue
		}
		if len(block.Succs) == 0 {
			return false
		}
		queue = append(queue, block.Succs...)
	}
	return panicked
}

func registryBlockPanics(pass *analysis.Pass, block *cfg.Block) bool {
	for _, node := range block.Nodes {
		if nodeHasCall(node, func(call *ast.CallExpr) bool { return proofPanic(pass, call) }) {
			return true
		}
	}
	return false
}

func retiredOwnerPreserved(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, target ast.Node, provider, owner *types.Var) bool {
	found := false
	ast.Inspect(info.decl.Body, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 || !flow.dominates(assign, target) {
			return true
		}
		r, ok := registryReferenceFor(pass, info.decl.Body, resolver, assign.Lhs[0], 0)
		if !ok || r.provider != provider || r.field != retiredRegistryField || !preservingRegistryAppend(pass, info, resolver, r, assign.Rhs[0]) {
			return true
		}
		call, _ := ast.Unparen(assign.Rhs[0]).(*ast.CallExpr)
		found = len(call.Args) == 2 && sameResolvedValue(resolver, call.Args[1], owner, "")
		return true
	})
	return found
}

func pruneAppendOwner(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, r registryReference, rhs ast.Expr) *types.Var {
	call, ok := ast.Unparen(rhs).(*ast.CallExpr)
	if !ok || !builtinCall(pass, call, "append") || len(call.Args) != 2 {
		return nil
	}
	base, ok := registryReferenceFor(pass, info.decl.Body, resolver, call.Args[0], 0)
	if !ok || !sameRegistry(r, base) || !base.sliced {
		return nil
	}
	id, ok := ast.Unparen(call.Args[1]).(*ast.Ident)
	if !ok {
		return nil
	}
	owner, _ := pass.TypesInfo.Uses[id].(*types.Var)
	return owner
}

func registryPruneProven(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, target *ast.AssignStmt, r registryReference, rhs ast.Expr) bool {
	id, ok := ast.Unparen(rhs).(*ast.Ident)
	if !ok {
		return false
	}
	kept, _ := pass.TypesInfo.Uses[id].(*types.Var)
	if kept == nil || assignmentsTo(pass, info.decl.Body, kept) != 2 {
		return false
	}
	if !pruneStartsEmpty(pass, info, resolver, kept, r) {
		return false
	}
	proven := false
	ast.Inspect(info.decl.Body, func(node ast.Node) bool {
		loop, ok := node.(*ast.RangeStmt)
		if !ok || !flow.dominates(loop.X, target) {
			return true
		}
		owner := pruneRangeOwner(pass, info, resolver, loop, r)
		if owner != nil {
			proven = pruneLoopRetains(pass, info, resolver, flow, loop, kept, owner)
		}
		return true
	})
	return proven
}
func pruneRangeOwner(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, loop *ast.RangeStmt, r registryReference) *types.Var {
	base, ok := registryReferenceFor(pass, info.decl.Body, resolver, loop.X, 0)
	if !ok || !sameRegistry(r, base) || base.sliced {
		return nil
	}
	id, ok := loop.Value.(*ast.Ident)
	if !ok {
		return nil
	}
	owner, _ := pass.TypesInfo.Defs[id].(*types.Var)
	return owner
}

func pruneStartsEmpty(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, kept *types.Var, r registryReference) bool {
	slice, ok := ast.Unparen(definitionExpr(pass, info.decl.Body, kept)).(*ast.SliceExpr)
	if !ok || slice.Low != nil || slice.Max != nil {
		return false
	}
	base, ok := registryReferenceFor(pass, info.decl.Body, resolver, slice.X, 0)
	if !ok || !sameRegistry(r, base) || base.sliced {
		return false
	}
	v, known := proofValue(pass, info.decl.Body, resolver, slice.High, nil)
	return known && v == 0
}

func pruneLoopRetains(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, flow *functionFlow, loop *ast.RangeStmt, kept, owner *types.Var) bool {
	if !pruneOwnerReadsLocked(pass, resolver, flow, loop, owner) {
		return false
	}
	start := pruneRangeBody(flow, loop)
	if start == nil {
		return false
	}
	type state struct {
		block    *cfg.Block
		retained bool
	}
	queue := []state{{block: start}}
	seen := make(map[state]bool)
	values := proofValues{{root: owner, path: mutationStopPath}: 1}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if seen[current] || !current.block.Live {
			continue
		}
		seen[current] = true
		boundary, valid := pruneIterationBoundary(current.block, loop, current.retained)
		if !valid {
			return false
		}
		if boundary {
			continue
		}
		retained := current.retained || blockRetainsOwner(pass, info, resolver, current.block, kept, owner)
		for _, next := range proofSuccessors(pass, info.decl.Body, resolver, current.block, values) {
			queue = append(queue, state{block: next, retained: retained})
		}
	}
	return true
}

func pruneRangeBody(flow *functionFlow, loop *ast.RangeStmt) *cfg.Block {
	for _, block := range flow.graph.Blocks {
		if block.Kind == cfg.KindRangeBody && block.Stmt == loop {
			return block
		}
	}
	return nil
}
func pruneIterationBoundary(block *cfg.Block, loop *ast.RangeStmt, retained bool) (bool, bool) {
	if block.Kind == cfg.KindRangeLoop && block.Stmt == loop {
		return true, retained
	}
	return false, pruneIterationBlock(block, loop)
}

func pruneIterationBlock(block *cfg.Block, loop *ast.RangeStmt) bool {
	if len(block.Succs) == 0 {
		return false
	}
	if block.Kind == cfg.KindRangeDone && block.Stmt == loop {
		return false
	}
	for _, node := range block.Nodes {
		if !nodeContains(loop.Body, node) {
			return false
		}
	}
	return true
}

func pruneOwnerReadsLocked(pass *analysis.Pass, resolver *pathResolver, flow *functionFlow, loop *ast.RangeStmt, owner *types.Var) bool {
	valid := true
	ast.Inspect(loop.Body, func(node ast.Node) bool {
		sel, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		root, path, ok := resolver.resolveExpr(sel)
		if ok && root == owner && path == mutationStopPath && !ownerMutexHeldAt(pass, resolver, flow, loop.Body, owner, sel) {
			valid = false
		}
		return true
	})
	return valid
}

func ownerMutexHeldAt(pass *analysis.Pass, resolver *pathResolver, flow *functionFlow, body *ast.BlockStmt, owner *types.Var, target ast.Node) bool {
	var locked, unlocked token.Pos
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || call.Pos() >= target.Pos() {
			return true
		}
		sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
		if !ok || !sameResolvedValue(resolver, sel.X, owner, ".mu") {
			return true
		}
		fn := calledFunction(pass, call)
		if isMethod(fn, "sync", "Mutex", "Lock") && flow.dominates(call, target) {
			locked = max(locked, call.Pos())
		}
		if isMethod(fn, "sync", "Mutex", "Unlock") {
			unlocked = max(unlocked, call.Pos())
		}
		return true
	})
	return locked > unlocked
}

func blockRetainsOwner(pass *analysis.Pass, info *functionInfo, resolver *pathResolver, block *cfg.Block, kept, owner *types.Var) bool {
	for _, node := range block.Nodes {
		assign, ok := node.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			continue
		}
		id, ok := assign.Lhs[0].(*ast.Ident)
		if !ok || pass.TypesInfo.Uses[id] != kept {
			continue
		}
		r, ok := registryReferenceFor(pass, info.decl.Body, resolver, id, 0)
		if ok && pruneAppendOwner(pass, info, resolver, r, assign.Rhs[0]) == owner {
			return true
		}
	}
	return false
}
