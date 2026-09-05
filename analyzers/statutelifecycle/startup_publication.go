package statutelifecycle

import (
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/cfg"
)

func propagatePublishers(pass *analysis.Pass, functions map[*types.Func]*functionInfo) {
	for changed := true; changed; {
		changed = false
		for _, info := range functions {
			if info.publishes || !containsPublisher(pass, info.decl.Body, functions, nil) {
				continue
			}
			info.publishes = true
			changed = true
		}
	}
}

// containsPublisher reports whether root publishes serving; exempt, when non-nil, excuses individual calls (per-call, never per-node) so a sibling unowned publisher still counts.
//
//nolint:gocyclo // publisher propagation deliberately distinguishes calls, helpers, and launched closures.
func containsPublisher(pass *analysis.Pass, root ast.Node, functions map[*types.Func]*functionInfo, exempt func(*ast.CallExpr) bool) bool {
	found := false
	ast.Inspect(root, func(node ast.Node) bool {
		if node == nil || found {
			return false
		}
		switch n := node.(type) {
		case *ast.FuncLit:
			// A function literal is inert unless a surrounding GoStmt launches it.
			return false
		case *ast.GoStmt:
			if (exempt == nil || !exempt(n.Call)) && callPublishes(pass, n.Call, functions) {
				found = true
				return false
			}
			if lit, ok := n.Call.Fun.(*ast.FuncLit); ok && containsPublisher(pass, lit.Body, functions, exempt) {
				found = true
			}
			return false
		case *ast.CallExpr:
			if exempt != nil && exempt(n) {
				return true
			}
			if callPublishes(pass, n, functions) {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

func callPublishes(pass *analysis.Pass, call *ast.CallExpr, functions map[*types.Func]*functionInfo) bool {
	fn := calledFunction(pass, call)
	if fn == nil {
		return false
	}
	if isServeFunction(fn) {
		return true
	}
	if info := functions[fn]; info != nil {
		return info.publishes
	}
	return false
}

//nolint:gocyclo // the small allowlist is safer than name-only Serve heuristics.
func isServeFunction(fn *types.Func) bool {
	switch fn.Name() {
	case "Serve", "ServeTLS", "ListenAndServe", "ListenAndServeTLS", "ListenAndServeQUIC":
	default:
		return false
	}
	sig, _ := fn.Type().(*types.Signature)
	if sig == nil || sig.Recv() == nil || !signatureReturnsError(sig) {
		return false
	}
	return isAllowlistedServerType(namedType(sig.Recv().Type()))
}

// isAllowlistedServerType reports whether named is a server type whose Serve family publishes and whose Shutdown/Close stops.
func isAllowlistedServerType(named *types.Named) bool {
	if named == nil || named.Obj().Pkg() == nil {
		return false
	}
	pkg := named.Obj().Pkg().Path()
	name := named.Obj().Name()
	return (pkg == "net/http" && name == "Server") ||
		(pkg == "github.com/quic-go/quic-go/http3" && name == "Server")
}

// isServerStopFunction reports whether fn is Shutdown or Close on an allowlisted server type.
func isServerStopFunction(fn *types.Func) bool {
	switch fn.Name() {
	case methodShutdown, methodClose:
	default:
		return false
	}
	sig, _ := fn.Type().(*types.Signature)
	if sig == nil || sig.Recv() == nil {
		return false
	}
	return isAllowlistedServerType(namedType(sig.Recv().Type()))
}

//nolint:gocyclo // CFG state traversal is clearer as one publication/commit state machine.
func checkPublishBeforeFailure(pass *analysis.Pass, info *functionInfo, functions map[*types.Func]*functionInfo) {
	if !isStartupFunction(info.fn.Name()) || !functionReturnsError(info.fn) || info.decl.Body == nil {
		return
	}
	graph := cfg.New(info.decl.Body, assumeCallReturns)
	if len(graph.Blocks) == 0 {
		return
	}
	roots := collectRollbackRoots(pass, info, functions)

	type state struct {
		block     *cfg.Block
		published bool
		committed bool
		rollback  uint64
	}
	queue := []state{{block: graph.Blocks[0]}}
	seen := make(map[state]bool)

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if current.block == nil || seen[current] {
			continue
		}
		seen[current] = true

		published := current.published
		committed := current.committed
		rollback := current.rollback
		for _, node := range current.block.Nodes {
			// A Serve call returned directly is the runtime loop itself, leaving
			// no later startup operation that can fail.
			if ret, ok := node.(*ast.ReturnStmt); ok && published && !committed && returnMayFail(info.fn, ret) {
				pass.Reportf(info.decl.Name.Pos(),
					"["+diagnosticSLC100+"] %s can publish serving before a later error return; bind/acquire every fallible startup resource before launching Serve",
					info.fn.Name())
				return
			}
			if commitsStart(node) {
				committed = true
				published = false
			}
			rollback |= deferredRollbackBits(pass, node, roots)
			exempt := func(call *ast.CallExpr) bool {
				return rollbackOwnedCall(pass, call, roots, rollback, functions)
			}
			if !committed && containsPublisher(pass, node, functions, exempt) {
				published = true
			}
		}

		for _, succ := range current.block.Succs {
			queue = append(queue, state{block: succ, published: published, committed: committed, rollback: rollback})
		}
	}
}

// rollbackRoot is a variable eligible to own a rollback-owned early publication, together with the owner types its rollback provably stops and awaits.
type rollbackRoot struct {
	v      *types.Var
	owners map[*types.TypeName]bool
}

// collectRollbackRoots finds the variables eligible to own a rollback-owned early publication: each roots a deferred rollback call somewhere in the body, and that rollback provably stops and awaits at least one owner's allowlisted server.
func collectRollbackRoots(pass *analysis.Pass, info *functionInfo, functions map[*types.Func]*functionInfo) []rollbackRoot {
	var roots []rollbackRoot
	seen := make(map[*types.Var]bool)
	ast.Inspect(info.decl.Body, func(node ast.Node) bool {
		ds, ok := node.(*ast.DeferStmt)
		if !ok {
			return true
		}
		for _, v := range deferredRollbackVars(pass, ds) {
			if seen[v] || len(roots) >= 64 {
				continue
			}
			seen[v] = true
			owners := rollbackStoppedOwners(pass, v.Type(), functions)
			if len(owners) > 0 {
				roots = append(roots, rollbackRoot{v: v, owners: owners})
			}
		}
		return true
	})
	return roots
}

// deferredRollbackVars returns the variables whose rollback ds registers, directly (defer a.rollback()) or inside the deferred function literal.
func deferredRollbackVars(pass *analysis.Pass, ds *ast.DeferStmt) []*types.Var {
	if v := rollbackCallRoot(pass, ds.Call); v != nil {
		return []*types.Var{v}
	}
	lit, ok := ds.Call.Fun.(*ast.FuncLit)
	if !ok {
		return nil
	}
	var out []*types.Var
	ast.Inspect(lit.Body, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok {
			if v := rollbackCallRoot(pass, call); v != nil {
				out = append(out, v)
			}
		}
		return true
	})
	return out
}

// rollbackCallRoot returns the root variable when call invokes a method named rollback through a selector chain rooted at an identifier, else nil.
func rollbackCallRoot(pass *analysis.Pass, call *ast.CallExpr) *types.Var {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "rollback" {
		return nil
	}
	return selectorRootVar(pass, sel)
}

// selectorRootVar walks a selector chain to its root identifier and resolves it to a variable, else nil.
func selectorRootVar(pass *analysis.Pass, sel *ast.SelectorExpr) *types.Var {
	expr := sel.X
	for {
		switch x := expr.(type) {
		case *ast.SelectorExpr:
			expr = x.X
		case *ast.ParenExpr:
			expr = x.X
		case *ast.Ident:
			v, _ := pass.TypesInfo.Uses[x].(*types.Var)
			return v
		default:
			return nil
		}
	}
}

// deferredRollbackBits returns the root bits a traversed defer statement registers; ordering matters, so only defers already reached in the CFG arm the exemption.
func deferredRollbackBits(pass *analysis.Pass, node ast.Node, roots []rollbackRoot) uint64 {
	ds, ok := node.(*ast.DeferStmt)
	if !ok {
		return 0
	}
	var bits uint64
	for _, v := range deferredRollbackVars(pass, ds) {
		for i, root := range roots {
			if v == root.v {
				bits |= 1 << i
			}
		}
	}
	return bits
}

// rollbackOwnedCall reports whether call is attempt-bracketed: rooted at a variable whose qualifying rollback defer was already traversed, with every owner type whose server the call launches stopped and awaited by that rollback.
func rollbackOwnedCall(pass *analysis.Pass, call *ast.CallExpr, roots []rollbackRoot, registered uint64, functions map[*types.Func]*functionInfo) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	v := selectorRootVar(pass, sel)
	if v == nil {
		return false
	}
	for i, root := range roots {
		if v != root.v {
			continue
		}
		if registered&(1<<i) == 0 {
			return false
		}
		published, resolved := publishedOwners(pass, call, functions)
		if !resolved || len(published) == 0 {
			return false
		}
		for owner := range published {
			if !root.owners[owner] {
				return false
			}
		}
		return true
	}
	return false
}

// publishedOwners resolves the owner types of every allowlisted Serve call reachable from call through local callees and function literals; resolved is false when any reachable Serve has no field-selected owner.
func publishedOwners(pass *analysis.Pass, call *ast.CallExpr, functions map[*types.Func]*functionInfo) (owners map[*types.TypeName]bool, resolved bool) {
	owners = make(map[*types.TypeName]bool)
	resolved = true
	seen := make(map[*types.Func]bool)
	var visitFn func(*types.Func)
	visitCall := func(c *ast.CallExpr) {
		fn := calledFunction(pass, c)
		if fn == nil {
			return
		}
		if !isServeFunction(fn) {
			visitFn(fn)
			return
		}
		var owner *types.TypeName
		if sel, ok := c.Fun.(*ast.SelectorExpr); ok {
			owner = fieldOwner(pass, sel.X)
		}
		if owner == nil {
			resolved = false
			return
		}
		owners[owner] = true
	}
	visitFn = func(fn *types.Func) {
		if seen[fn] {
			return
		}
		seen[fn] = true
		info := functions[fn]
		if info == nil || info.decl.Body == nil {
			return
		}
		ast.Inspect(info.decl.Body, func(node ast.Node) bool {
			if c, ok := node.(*ast.CallExpr); ok {
				visitCall(c)
			}
			return true
		})
	}
	visitCall(call)
	return owners, resolved
}

// rollbackStoppedOwners resolves owner's rollback via the method set and returns the owner types it provably stops and awaits.
func rollbackStoppedOwners(pass *analysis.Pass, owner types.Type, functions map[*types.Func]*functionInfo) map[*types.TypeName]bool {
	root := lookupMethod(owner, "rollback")
	if root == nil {
		return nil
	}
	return stoppedOwners(pass, root, functions)
}

// stoppedOwners runs the stopper fixpoint from root; an owner type qualifies only when one body in the transitive call closure both stops that owner's server and shows that same owner's completion-wait evidence.
func stoppedOwners(pass *analysis.Pass, root *types.Func, functions map[*types.Func]*functionInfo) map[*types.TypeName]bool {
	owners := make(map[*types.TypeName]bool)
	seen := make(map[*types.Func]bool)
	queue := []*types.Func{root}
	for len(queue) > 0 {
		fn := queue[0]
		queue = queue[1:]
		if seen[fn] {
			continue
		}
		seen[fn] = true
		info := functions[fn]
		if info == nil || info.decl.Body == nil {
			continue
		}
		stops, waits := bodyOwnerEvidence(pass, info.decl.Body)
		for owner := range stops {
			if waits[owner] {
				owners[owner] = true
			}
		}
		queue = append(queue, bodyCallees(pass, info.decl.Body)...)
	}
	return owners
}

// bodyOwnerEvidence collects per-owner evidence in one body: owner types whose allowlisted server is stopped, and owner types with completion-wait evidence (a receive from the owner's channel field or the owner's WaitGroup.Wait); function literals stay inert except sync.Once.Do bodies, per the SLC103 evidence machinery.
func bodyOwnerEvidence(pass *analysis.Pass, body *ast.BlockStmt) (stops, waits map[*types.TypeName]bool) {
	stops = make(map[*types.TypeName]bool)
	waits = make(map[*types.TypeName]bool)
	var inspect func(ast.Node)
	inspect = func(root ast.Node) {
		ast.Inspect(root, func(node ast.Node) bool {
			switch n := node.(type) {
			case nil, *ast.FuncLit:
				return false
			case *ast.UnaryExpr:
				if n.Op == token.ARROW {
					markOwner(waits, fieldOwner(pass, n.X))
				}
			case *ast.CallExpr:
				scanOwnerCall(pass, n, stops, waits, inspect)
			}
			return true
		})
	}
	inspect(body)
	return stops, waits
}

// scanOwnerCall records call's per-owner stop or wait evidence and follows sync.Once.Do bodies via inspect.
func scanOwnerCall(pass *analysis.Pass, call *ast.CallExpr, stops, waits map[*types.TypeName]bool, inspect func(ast.Node)) {
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		if callee := calledFunction(pass, call); callee != nil && isServerStopFunction(callee) {
			markOwner(stops, fieldOwner(pass, sel.X))
		}
		if isSyncMethodCall(pass, call, "WaitGroup", "Wait") {
			markOwner(waits, fieldOwner(pass, sel.X))
		}
	}
	if isSyncMethodCall(pass, call, "Once", "Do") {
		for _, arg := range call.Args {
			if lit, ok := arg.(*ast.FuncLit); ok {
				inspect(lit.Body)
			}
		}
	}
}

// markOwner adds owner to set unless the evidence had no resolvable owner.
func markOwner(set map[*types.TypeName]bool, owner *types.TypeName) {
	if owner != nil {
		set[owner] = true
	}
}

// bodyCallees returns every function body calls, function literals included, for the stopper fixpoint.
func bodyCallees(pass *analysis.Pass, body *ast.BlockStmt) []*types.Func {
	var callees []*types.Func
	ast.Inspect(body, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok {
			if callee := calledFunction(pass, call); callee != nil {
				callees = append(callees, callee)
			}
		}
		return true
	})
	return callees
}

// fieldOwner returns the named type owning expr's selected field (for p.f, the named type of p): the correlation granularity for publish, stop, and wait evidence; nil when expr is not a field selection or the parent type is unnamed.
func fieldOwner(pass *analysis.Pass, expr ast.Expr) *types.TypeName {
	sel, ok := ast.Unparen(expr).(*ast.SelectorExpr)
	if !ok {
		return nil
	}
	named := namedType(pass.TypesInfo.TypeOf(sel.X))
	if named == nil {
		return nil
	}
	return named.Obj()
}

// lookupMethod resolves name in the method sets of owner and its pointer type.
func lookupMethod(owner types.Type, name string) *types.Func {
	sets := []*types.MethodSet{types.NewMethodSet(owner)}
	if _, pointer := types.Unalias(owner).(*types.Pointer); !pointer {
		if named := namedType(owner); named != nil {
			sets = append(sets, types.NewMethodSet(types.NewPointer(named)))
		}
	}
	for _, set := range sets {
		for method := range set.Methods() {
			if fn, _ := method.Obj().(*types.Func); fn != nil && fn.Name() == name {
				return fn
			}
		}
	}
	return nil
}

func isStartupFunction(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, methodStart) || strings.HasPrefix(lower, "bind")
}

func commitsStart(node ast.Node) bool {
	assign, ok := node.(*ast.AssignStmt)
	if !ok {
		return false
	}
	for i, lhs := range assign.Lhs {
		sel, ok := lhs.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "started" || i >= len(assign.Rhs) {
			continue
		}
		id, ok := assign.Rhs[i].(*ast.Ident)
		if ok && id.Name == "true" {
			return true
		}
	}
	return false
}

//nolint:gocyclo // return-shape handling stays explicit to avoid false lifecycle diagnostics.
func returnMayFail(fn *types.Func, ret *ast.ReturnStmt) bool {
	sig, _ := fn.Type().(*types.Signature)
	if sig == nil || !signatureReturnsError(sig) {
		return false
	}
	if len(ret.Results) == 0 {
		// Named error results can carry a failure through a naked return.
		return true
	}
	if len(ret.Results) != sig.Results().Len() {
		// A multi-valued expression can feed several results; stay conservative.
		return true
	}
	errType := types.Universe.Lookup("error").Type()
	for i, expr := range ret.Results {
		if !types.AssignableTo(sig.Results().At(i).Type(), errType) && !types.Identical(sig.Results().At(i).Type(), errType) {
			continue
		}
		if id, ok := expr.(*ast.Ident); ok && id.Name == "nil" {
			continue
		}
		return true
	}
	return false
}
