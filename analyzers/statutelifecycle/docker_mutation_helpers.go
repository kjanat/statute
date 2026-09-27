package statutelifecycle

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/cfg"
)

// helperTruth carries both possibilities when a branch is outside the proof model.
type helperTruth uint8

const (
	mutationStopPath         = ".stop"
	mutationBindingPath      = ".binding"
	mutationBindingKeyPath   = ".binding.key"
	mutationContainerIDPath  = ".binding.containerID"
	mutationResultPath       = ".result"
	mutationRunningPath      = ".Running"
	mutationResultField      = "result"
	mutationOwnerIDPath      = ".containerID"
	mutationBindingField     = "binding"
	mutationContainerIDField = "containerID"
	mutationObservedIDPath   = ".ContainerID"
	mutationActivateMethod   = "activate"
)

const (
	helperFalse helperTruth = 1 << iota
	helperTrue
	helperUnknown = helperFalse | helperTrue
)

func helperBool(value bool) helperTruth {
	if value {
		return helperTrue
	}
	return helperFalse
}

func checkDockerMutationHelpers(pass *analysis.Pass, functions map[*types.Func]*functionInfo, parents map[ast.Node]ast.Node) {
	for _, info := range functions {
		switch {
		case isLocalMethod(info.fn, "workload", "callRef"):
			checkMutationReferenceHelper(pass, info, false)
		case isLocalMethod(info.fn, "workloadBinding", "ref"):
			checkMutationReferenceHelper(pass, info, true)
		case isLocalMethod(info.fn, "dockerProvider", "persistOwnedStop"):
			checkPersistenceHelper(pass, info, parents)
		case isLocalMethod(info.fn, "workload", "stopOwnershipLocked"):
			checkOwnershipHelper(pass, info, false)
		case isLocalMethod(info.fn, "workloadStopOwnership", "currentLocked"):
			checkOwnershipHelper(pass, info, true)
		case isLocalMethod(info.fn, "dockerProvider", "attemptOwnedStop"):
			checkAttemptClassification(pass, info, parents)
		case isLocalMethod(info.fn, "workload", "stopResult"):
			checkStopResultHelper(pass, info)
		}
		checkBindingIdentityHelpers(pass, info)
	}
}

// helperBranches evaluates only recognized predicates; unknown calls keep both edges.
func helperBranches(pass *analysis.Pass, body *ast.BlockStmt, expr ast.Expr, atom func(ast.Expr) helperTruth) helperTruth {
	expr = ast.Unparen(stableDefinitionExpr(pass, body, expr, 0))
	if value := pass.TypesInfo.Types[expr].Value; value != nil && value.Kind() == constant.Bool {
		return helperBool(constant.BoolVal(value))
	}
	if unary, ok := expr.(*ast.UnaryExpr); ok && unary.Op == token.NOT {
		value := helperBranches(pass, body, unary.X, atom)
		return (value&helperFalse)<<1 | (value&helperTrue)>>1
	}
	if binary, ok := expr.(*ast.BinaryExpr); ok && (binary.Op == token.LAND || binary.Op == token.LOR) {
		left := helperBranches(pass, body, binary.X, atom)
		right := helperBranches(pass, body, binary.Y, atom)
		return helperCombine(left, right, binary.Op)
	}
	return atom(expr)
}

func helperCombine(left, right helperTruth, op token.Token) helperTruth {
	var result helperTruth
	for _, a := range []helperTruth{helperFalse, helperTrue} {
		for _, b := range []helperTruth{helperFalse, helperTrue} {
			if left&a == 0 || right&b == 0 {
				continue
			}
			value := a == helperTrue && b == helperTrue
			if op == token.LOR {
				value = a == helperTrue || b == helperTrue
			}
			result |= helperBool(value)
		}
	}
	return result
}

func helperReachable(flow *functionFlow, target ast.Node, condition func(ast.Expr) helperTruth) bool {
	point, ok := flow.point(target)
	if !ok {
		return true
	}
	queue := []*cfg.Block{flow.graph.Blocks[0]}
	seen := make(map[*cfg.Block]bool)
	for len(queue) > 0 {
		block := queue[0]
		queue = queue[1:]
		if !block.Live || seen[block] {
			continue
		}
		seen[block] = true
		if block == point.block {
			return true
		}
		queue = append(queue, helperSuccessors(block, condition)...)
	}
	return false
}

func helperSuccessors(block *cfg.Block, condition func(ast.Expr) helperTruth) []*cfg.Block {
	if len(block.Succs) != 2 || len(block.Nodes) == 0 {
		return block.Succs
	}
	expr, ok := block.Nodes[len(block.Nodes)-1].(ast.Expr)
	if !ok {
		return block.Succs
	}
	value := condition(expr)
	if value == helperTrue {
		return block.Succs[:1]
	}
	if value == helperFalse {
		return block.Succs[1:]
	}
	return block.Succs
}

type referenceProof struct {
	pass     *analysis.Pass
	info     *functionInfo
	resolver *pathResolver
	sig      *types.Signature
	binding  bool
	present  bool
	matching bool
	nonempty bool
}

func checkMutationReferenceHelper(pass *analysis.Pass, info *functionInfo, binding bool) {
	sig, _ := info.fn.Type().(*types.Signature)
	if sig == nil || sig.Recv() == nil || (!binding && sig.Params().Len() != 2) {
		return
	}
	// Reduced fixtures without binding state exercise call boundaries separately.
	if !binding && !helperHasField(sig.Recv().Type(), "binding") {
		return
	}
	proof := referenceProof{pass: pass, info: info, resolver: newPathResolver(pass, info.decl.Body), sig: sig, binding: binding}
	flow := newFunctionFlow(info.decl.Body)
	ast.Inspect(info.decl.Body, func(node ast.Node) bool {
		return proof.checkNode(node, flow)
	})
}

func (p referenceProof) checkNode(node ast.Node, flow *functionFlow) bool {
	if _, literal := node.(*ast.FuncLit); literal {
		p.pass.Reportf(node.Pos(), "[%s] mutation reference helper cannot hide binding effects in a closure", diagnosticSLC105)
		return false
	}
	if call, ok := node.(*ast.CallExpr); ok && !helperLockCall(p.pass, call) && !p.bindingReference(call) {
		p.pass.Reportf(call.Pos(), "[%s] mutation reference helper calls must preserve binding identity", diagnosticSLC105)
	}
	ret, ok := node.(*ast.ReturnStmt)
	if !ok {
		return true
	}
	for scenario := range 8 {
		p.present, p.matching, p.nonempty = scenario&1 != 0, scenario&2 != 0, scenario&4 != 0
		if helperReachable(flow, ret, p.condition) && !p.validReturn(ret) {
			p.pass.Reportf(ret.Pos(), "[%s] mutation reference helper must preserve the operation binding and prefer its immutable container ID", diagnosticSLC105)
			break
		}
	}
	return true
}

func helperLockCall(pass *analysis.Pass, call *ast.CallExpr) bool {
	for _, owner := range []string{"Mutex", "RWMutex"} {
		for _, method := range []string{"Lock", "Unlock", "RLock", "RUnlock"} {
			if isSyncMethodCall(pass, call, owner, method) {
				return true
			}
		}
	}
	return false
}

func helperHasField(t types.Type, name string) bool {
	named := namedType(t)
	if named == nil {
		return false
	}
	structure, ok := named.Underlying().(*types.Struct)
	if !ok {
		return false
	}
	for field := range structure.Fields() {
		if field.Name() == name {
			return true
		}
	}
	return false
}

func (p referenceProof) value(expr ast.Expr, root *types.Var, path string) bool {
	expr = stableDefinitionExpr(p.pass, p.info.decl.Body, expr, 0)
	return sameResolvedValue(p.resolver, expr, root, path)
}

func (p referenceProof) bindingReference(expr ast.Expr) bool {
	expr = stableDefinitionExpr(p.pass, p.info.decl.Body, expr, 0)
	call, ok := ast.Unparen(expr).(*ast.CallExpr)
	if !ok || !isLocalMethod(calledFunction(p.pass, call), "workloadBinding", "ref") || len(call.Args) != 0 {
		return false
	}
	sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	return ok && p.value(sel.X, p.sig.Recv(), mutationBindingPath)
}

func helperEmpty(pass *analysis.Pass, expr ast.Expr) bool {
	value := pass.TypesInfo.Types[ast.Unparen(expr)].Value
	return value != nil && value.Kind() == constant.String && constant.StringVal(value) == ""
}

func (p referenceProof) condition(expr ast.Expr) helperTruth {
	return helperBranches(p.pass, p.info.decl.Body, expr, p.atom)
}

func (p referenceProof) atom(expr ast.Expr) helperTruth {
	binary, ok := expr.(*ast.BinaryExpr)
	if !ok || (binary.Op != token.EQL && binary.Op != token.NEQ) {
		return helperUnknown
	}
	for _, pair := range [][2]ast.Expr{{binary.X, binary.Y}, {binary.Y, binary.X}} {
		if equal, known := p.equal(pair[0], pair[1]); known {
			return helperBool(equal == (binary.Op == token.EQL))
		}
	}
	return helperUnknown
}

func (p referenceProof) equal(left, right ast.Expr) (bool, bool) {
	path := mutationBindingPath
	if p.binding {
		path = ""
	}
	if p.value(left, p.sig.Recv(), path) && isNil(p.pass, right) {
		return !p.present, true
	}
	if p.binding {
		return !p.nonempty, p.value(left, p.sig.Recv(), ".containerID") && helperEmpty(p.pass, right)
	}
	if p.value(left, p.sig.Recv(), mutationBindingKeyPath) && p.value(right, p.sig.Params().At(0), "") {
		return p.matching, true
	}
	return !p.nonempty, p.bindingReference(left) && helperEmpty(p.pass, right)
}

func (p referenceProof) validReturn(ret *ast.ReturnStmt) bool {
	if len(ret.Results) != 1 {
		return false
	}
	expr := stableDefinitionExpr(p.pass, p.info.decl.Body, ret.Results[0], 0)
	if p.binding {
		if !p.present {
			return helperEmpty(p.pass, expr)
		}
		if p.nonempty {
			return p.value(expr, p.sig.Recv(), ".containerID")
		}
		return p.value(expr, p.sig.Recv(), ".container")
	}
	if p.present && p.matching && p.nonempty {
		return p.bindingReference(expr)
	}
	return p.value(expr, p.sig.Params().At(1), "")
}

type persistenceProof struct {
	pass         *analysis.Pass
	info         *functionInfo
	resolver     *pathResolver
	sig          *types.Signature
	capture      *ast.AssignStmt
	owner        *types.Var
	owned        *types.Var
	put          *ast.CallExpr
	putError     *types.Var
	validPut     bool
	flow         *functionFlow
	immutableRef ast.Node
}

func checkPersistenceHelper(pass *analysis.Pass, info *functionInfo, parents map[ast.Node]ast.Node) {
	sig, _ := info.fn.Type().(*types.Signature)
	if sig == nil || sig.Recv() == nil || sig.Params().Len() != 2 || !helperHasField(sig.Params().At(1).Type(), "persisted") {
		return
	}
	proof := persistenceProof{pass: pass, info: info, resolver: newPathResolver(pass, info.decl.Body), sig: sig, flow: newFunctionFlow(info.decl.Body)}
	proof.collect(parents)
	flow := proof.flow
	if proof.put != nil && !proof.referenceCapturedBeforePut(flow) {
		pass.Reportf(proof.put.Pos(), "[%s] persistence must capture the owned immutable fallback before registry insertion", diagnosticSLC105)
	}
	ast.Inspect(info.decl.Body, func(node ast.Node) bool {
		return proof.checkNode(node, flow)
	})
}

func (p persistenceProof) checkNode(node ast.Node, flow *functionFlow) bool {
	if _, literal := node.(*ast.FuncLit); literal {
		return false
	}
	if assign, ok := node.(*ast.AssignStmt); ok {
		p.checkPersistedWrite(assign, flow)
		p.checkReferenceCapture(assign)
	}
	ret, ok := node.(*ast.ReturnStmt)
	if !ok || p.definitelyError(ret) {
		return true
	}
	if p.capture == nil || !flow.dominates(p.capture, ret) || !p.validReturn(flow, ret) {
		p.pass.Reportf(ret.Pos(), "[%s] persistence success requires the same owned stop and successful registry write followed by owner revalidation", diagnosticSLC106)
	}
	return true
}

func (p persistenceProof) checkPersistedWrite(assign *ast.AssignStmt, flow *functionFlow) {
	for _, lhs := range assign.Lhs {
		if p.value(lhs, p.sig.Params().At(1), ".persisted") && !p.validSuccess(flow, assign, nil, false) {
			p.pass.Reportf(assign.Pos(), "[%s] persisted flag requires successful registry write followed by owner revalidation", diagnosticSLC106)
		}
	}
}

func (p *persistenceProof) collect(parents map[ast.Node]ast.Node) {
	ast.Inspect(p.info.decl.Body, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok || enclosedByFuncLiteral(node, p.info.decl.Body, parents) || len(assign.Rhs) != 1 {
			return true
		}
		call, ok := ast.Unparen(assign.Rhs[0]).(*ast.CallExpr)
		if !ok || insideDeferredOrGo(call, p.info.decl.Body, parents) {
			return true
		}
		p.collectCall(assign, call)
		return true
	})
	if p.put != nil && p.owner != nil {
		p.validPut = p.registryReceiver(p.put) && p.record(p.put.Args[0])
	}
	p.collectReference()
}

func (p *persistenceProof) collectReference() {
	ast.Inspect(p.info.decl.Body, func(node ast.Node) bool {
		if assign, ok := node.(*ast.AssignStmt); ok && p.referenceCapture(assign) {
			p.immutableRef = assign
		}
		return true
	})
}

func (p *persistenceProof) collectCall(assign *ast.AssignStmt, call *ast.CallExpr) {
	if len(call.Args) != 1 {
		return
	}
	fn := calledFunction(p.pass, call)
	if isLocalMethod(fn, "workload", "stopOwnershipLocked") && len(assign.Lhs) == 2 {
		sel, selected := ast.Unparen(call.Fun).(*ast.SelectorExpr)
		if selected && p.value(sel.X, p.sig.Params().At(0), "") && p.value(call.Args[0], p.sig.Params().At(1), "") {
			p.capture, p.owner, p.owned = assign, helperDefinedVar(p.pass, assign.Lhs[0]), helperDefinedVar(p.pass, assign.Lhs[1])
		}
	}
	if isLocalMethod(fn, "mutationRegistry", "put") && len(assign.Lhs) == 1 {
		p.put, p.putError = call, helperDefinedVar(p.pass, assign.Lhs[0])
	}
}

func helperDefinedVar(pass *analysis.Pass, expr ast.Expr) *types.Var {
	id, ok := expr.(*ast.Ident)
	if !ok {
		return nil
	}
	variable, _ := pass.TypesInfo.Defs[id].(*types.Var)
	return variable
}

func (p persistenceProof) value(expr ast.Expr, root *types.Var, path string) bool {
	if expr == nil {
		return false
	}
	actual, actualPath, ok := p.resolver.resolve(expr)
	if path == ".persisted" {
		actual, actualPath, ok = p.resolver.resolveExpr(expr)
	}
	if helperSameRoot(actual, actualPath, ok, root, path) {
		return true
	}
	definition := stableDefinitionExpr(p.pass, p.info.decl.Body, expr, 0)
	if definition == expr {
		return false
	}
	actual, actualPath, ok = p.resolver.resolve(definition)
	return helperSameRoot(actual, actualPath, ok, root, path)
}

func helperSameRoot(actual *types.Var, actualPath string, ok bool, root *types.Var, path string) bool {
	return root != nil && ok && actual == root && actualPath == path
}

func (p persistenceProof) registryReceiver(call *ast.CallExpr) bool {
	sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return false
	}
	expr := stableDefinitionExpr(p.pass, p.info.decl.Body, sel.X, 0)
	registry, ok := ast.Unparen(expr).(*ast.CallExpr)
	if !ok || !isLocalMethod(calledFunction(p.pass, registry), "dockerProvider", "currentMutationRegistry") {
		return false
	}
	provider, ok := ast.Unparen(registry.Fun).(*ast.SelectorExpr)
	return ok && p.value(provider.X, p.sig.Recv(), "")
}

func (p persistenceProof) record(expr ast.Expr) bool {
	expr = helperStableRecord(p.pass, p.info.decl.Body, p.resolver, expr)
	if expr == nil {
		return false
	}
	literal, _ := ast.Unparen(expr).(*ast.CompositeLit)
	if !helperLiteralType(p.pass, literal, "mutationRecord") {
		return false
	}
	identity, kind, prepared := false, false, false
	for _, element := range literal.Elts {
		key, value, ok := helperLiteralField(element)
		if !ok {
			return false
		}
		switch key {
		case "ContainerID":
			identity = p.value(value, p.owner, ".containerID")
		case "State":
			prepared = helperConstant(p.pass, value, "mutationRecordPrepared")
		case "Kind":
			kind = p.recordKind(value)
		}
	}
	return identity && kind && prepared
}

func helperLiteralType(pass *analysis.Pass, literal *ast.CompositeLit, name string) bool {
	return literal != nil && isNamedPackageType(pass.TypesInfo.TypeOf(literal), name)
}

func helperLiteralField(element ast.Expr) (string, ast.Expr, bool) {
	field, ok := element.(*ast.KeyValueExpr)
	if !ok {
		return "", nil, false
	}
	key, ok := field.Key.(*ast.Ident)
	if !ok {
		return "", nil, false
	}
	return key.Name, field.Value, true
}

func (p persistenceProof) recordKind(expr ast.Expr) bool {
	call, ok := ast.Unparen(expr).(*ast.CallExpr)
	return ok && len(call.Args) == 1 && isPackageFunction(calledFunction(p.pass, call), statutePackagePath, "mutationRecordKindForStop") && p.value(call.Args[0], p.sig.Params().At(1), ".kind")
}

func helperStableRecord(pass *analysis.Pass, body *ast.BlockStmt, resolver *pathResolver, expr ast.Expr) ast.Expr {
	for range 32 {
		root, _, ok := resolver.resolveExpr(expr)
		if !ok {
			return expr
		}
		if len(resolver.written[root]) != 0 || resolver.mutated[root] {
			return nil
		}
		definition := definitionExpr(pass, body, root)
		if definition == nil {
			return nil
		}
		expr = definition
	}
	return nil
}

func helperConstant(pass *analysis.Pass, expr ast.Expr, name string) bool {
	id, ok := ast.Unparen(expr).(*ast.Ident)
	if !ok {
		return false
	}
	value, _ := pass.TypesInfo.Uses[id].(*types.Const)
	return value != nil && value.Pkg() == pass.Pkg && value.Name() == name
}

func (p persistenceProof) definitelyError(ret *ast.ReturnStmt) bool {
	if len(ret.Results) != 1 {
		return false
	}
	expr := stableDefinitionExpr(p.pass, p.info.decl.Body, ret.Results[0], 0)
	call, ok := ast.Unparen(expr).(*ast.CallExpr)
	return ok && (isPackageFunction(calledFunction(p.pass, call), "errors", "New") || isPackageFunction(calledFunction(p.pass, call), "fmt", "Errorf"))
}

func (p persistenceProof) validReturn(flow *functionFlow, ret *ast.ReturnStmt) bool {
	if len(ret.Results) != 1 {
		return false
	}
	return p.validSuccess(flow, ret, ret.Results[0], true)
}

func (p persistenceProof) validSuccess(flow *functionFlow, target ast.Node, result ast.Expr, allowPersisted bool) bool {
	for scenario := range 16 {
		owned, persisted := scenario&1 != 0, scenario&2 != 0
		putOK, current := scenario&4 != 0, scenario&8 != 0
		if p.value(result, p.putError, "") && !putOK {
			continue
		}
		if !helperReachable(flow, target, func(expr ast.Expr) helperTruth {
			return helperBranches(p.pass, p.info.decl.Body, expr, func(atom ast.Expr) helperTruth {
				return p.atom(atom, owned, persisted, putOK, current)
			})
		}) {
			continue
		}
		if !owned || ((!allowPersisted || !persisted) && !p.completedWrite(flow, target, putOK, current)) {
			return false
		}
	}
	return true
}

func (p persistenceProof) completedWrite(flow *functionFlow, target ast.Node, putOK, current bool) bool {
	return putOK && current && p.validPut && flow.dominates(p.put, target)
}

func (p persistenceProof) referenceCapturedBeforePut(flow *functionFlow) bool {
	return !helperHasField(p.sig.Params().At(1).Type(), "ref") || p.immutableRef != nil && flow.dominates(p.immutableRef, p.put)
}

func (p persistenceProof) checkReferenceCapture(assign *ast.AssignStmt) {
	for _, lhs := range assign.Lhs {
		if protectedStateField(p.pass, p.resolver, lhs, "workloadStop", "ref") && !p.referenceCapture(assign) {
			p.pass.Reportf(assign.Pos(), "[%s] stop fallback must capture the owned immutable ID before first persistence", diagnosticSLC105)
		}
	}
}

func (p persistenceProof) referenceCapture(assign *ast.AssignStmt) bool {
	if len(assign.Lhs) != 1 || len(assign.Rhs) != 1 || p.capture == nil || !p.flow.dominates(p.capture, assign) {
		return false
	}
	if !sameBindingStorage(p.resolver, assign.Lhs[0], p.sig.Params().At(1), ".ref") || !p.value(assign.Rhs[0], p.owner, mutationOwnerIDPath) {
		return false
	}
	return p.referenceCaptureGuarded(assign)
}

func (p persistenceProof) referenceCaptureGuarded(assign *ast.AssignStmt) bool {
	for scenario := range 4 {
		owned, persisted := scenario&1 != 0, scenario&2 != 0
		if owned && !persisted {
			continue
		}
		condition := func(expr ast.Expr) helperTruth {
			return helperBranches(p.pass, p.info.decl.Body, expr, func(atom ast.Expr) helperTruth { return p.atom(atom, owned, persisted, false, false) })
		}
		if helperReachable(p.flow, assign, condition) {
			return false
		}
	}
	return true
}

func (p persistenceProof) atom(expr ast.Expr, owned, persisted, putOK, current bool) helperTruth {
	if p.value(expr, p.owned, "") {
		return helperBool(owned)
	}
	if p.value(expr, p.sig.Params().At(1), ".persisted") {
		return helperBool(persisted)
	}
	if p.revalidates(expr) {
		return helperBool(current)
	}
	if binary, ok := expr.(*ast.BinaryExpr); ok && (binary.Op == token.EQL || binary.Op == token.NEQ) {
		for _, pair := range [][2]ast.Expr{{binary.X, binary.Y}, {binary.Y, binary.X}} {
			if p.value(pair[0], p.putError, "") && isNil(p.pass, pair[1]) {
				return helperBool(putOK == (binary.Op == token.EQL))
			}
		}
	}
	return helperUnknown
}

func (p persistenceProof) revalidates(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 || !isLocalMethod(calledFunction(p.pass, call), "workloadStopOwnership", "currentLocked") {
		return false
	}
	sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	return ok && p.put != nil && p.flow.dominates(p.put, call) && !nodeContains(p.put, call) && p.value(sel.X, p.owner, "") && p.value(call.Args[0], p.sig.Params().At(0), "")
}
