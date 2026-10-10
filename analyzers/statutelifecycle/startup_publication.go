package statutelifecycle

import (
	"go/ast"
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
			if callPublishes(pass, n.Call, functions) {
				found = exempt == nil || !exempt(n.Call)
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
	if lit, ok := ast.Unparen(call.Fun).(*ast.FuncLit); ok {
		return containsPublisher(pass, lit.Body, functions, nil)
	}
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
	proofBody := startupFailureBody(info.fn, info.decl.Body)
	roots := collectRollbackRegistrations(pass, info, functions, proofBody)

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
			rollback |= deferredRollbackBits(node, roots)
			exempt := func(call *ast.CallExpr) bool {
				return rollbackOwnedCall(pass, call, roots, rollback, functions, proofBody)
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

// startupFailureBody bounds caller provenance to operations that can still
// precede an error return. Only an explicit successful return and its final
// top-level straight-line suffix are removed; helpers and the CFG stay intact.
func startupFailureBody(fn *types.Func, body *ast.BlockStmt) *ast.BlockStmt {
	if len(body.List) == 0 {
		return body
	}
	last := len(body.List) - 1
	ret, ok := body.List[last].(*ast.ReturnStmt)
	if !ok || len(ret.Results) == 0 || returnMayFail(fn, ret) {
		return body
	}
	for last > 0 && successTailStatement(body.List[last-1]) {
		last--
	}
	prefix := *body
	prefix.List = body.List[:last]
	return &prefix
}

func successTailStatement(stmt ast.Stmt) bool {
	switch stmt.(type) {
	case *ast.AssignStmt, *ast.ExprStmt, *ast.DeclStmt, *ast.IncDecStmt, *ast.EmptyStmt, *ast.GoStmt, *ast.SendStmt:
		return true
	default:
		return false // Control flow, labels, nested blocks and defers stay in the proof.
	}
}

// rollbackRegistration belongs to one defer occurrence, with exact cleanup identities.
type rollbackRegistration struct {
	deferStmt *ast.DeferStmt
	stops     map[groupKey]bool
	waits     map[groupKey]bool
}

// collectRollbackRegistrations summarizes actual deferred callees at their full
// receiver identities. Inert literals cannot register cleanup in this CFG.
func collectRollbackRegistrations(pass *analysis.Pass, info *functionInfo, functions map[*types.Func]*functionInfo, proofBody *ast.BlockStmt) []rollbackRegistration {
	var roots []rollbackRegistration
	ast.Inspect(info.decl.Body, func(node ast.Node) bool {
		if _, ok := node.(*ast.FuncLit); ok {
			return false
		}
		ds, ok := node.(*ast.DeferStmt)
		if !ok {
			return true
		}
		if len(roots) >= 64 {
			return false
		}
		stops, waits := rollbackResources(pass, ds.Call, functions, proofBody)
		if len(stops) > 0 && len(waits) > 0 {
			roots = append(roots, rollbackRegistration{deferStmt: ds, stops: stops, waits: waits})
		}
		return false
	})
	return roots
}

// deferredRollbackBits arms only the exact defer occurrence traversed in the CFG.
func deferredRollbackBits(node ast.Node, roots []rollbackRegistration) uint64 {
	ds, ok := node.(*ast.DeferStmt)
	if !ok {
		return 0
	}
	var bits uint64
	for i, root := range roots {
		if ds == root.deferStmt {
			bits |= 1 << i
		}
	}
	return bits
}

// rollbackOwnedCall requires one registered defer to stop every exact server
// published by this call and join its matching producer completion.
//
//nolint:gocyclo // every publication needs both exact stop and matching completion evidence from one registered owner.
func rollbackOwnedCall(pass *analysis.Pass, call *ast.CallExpr, roots []rollbackRegistration, registered uint64, functions map[*types.Func]*functionInfo, body *ast.BlockStmt) bool {
	publications, resolved := publicationResources(pass, call, functions, body)
	if !resolved || len(publications) == 0 {
		return false
	}
	for i, root := range roots {
		if registered&(1<<i) == 0 {
			continue
		}
		covered := true
		for _, publication := range publications {
			joined := false
			for signal := range publication.signals {
				joined = joined || root.waits[signal]
			}
			if !root.stops[publication.server] || !joined {
				covered = false
			}
		}
		if covered {
			return true
		}
	}
	return false
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
