// Package statutelifecycle provides Statute-specific lifecycle correctness checks.
package statutelifecycle

import (
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"github.com/golangci/plugin-module-register/register"
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/cfg"
)

const (
	pluginName          = "statutelifecycle"
	methodStart         = "start"
	methodStartAPI      = "Start"
	methodStop          = "stop"
	methodStopRun       = "stopRun"
	methodShutdownLocal = "shutdown"
	methodShutdown      = "Shutdown"
	methodClose         = "Close"
)

// Analyzer checks Statute-specific lifecycle ownership invariants.
// SLC100 exempts a rollback-owned early publication: a publication rooted at a value whose rollback is already deferred and provably stops and awaits every server it launches is attempt-bracketed, not a leak.
// The evidence is correlated per owner type (the named struct from which the server field, done channel, or WaitGroup is selected): each owner type whose server the publication launches must have one function body in the rollback's transitive call closure containing both a Shutdown/Close on that owner's server and a completion wait on that same owner; a stop and a wait belonging to different owners, or split across unrelated bodies, arm nothing.
var Analyzer = &analysis.Analyzer{
	Name: pluginName,
	Doc:  "trace Statute lifecycle publication, goroutine ownership, and cleanup invariants",
	Run:  run,
}

func init() {
	register.Plugin(pluginName, newPlugin)
}

type plugin struct{}

func newPlugin(any) (register.LinterPlugin, error) {
	return plugin{}, nil
}

func (plugin) BuildAnalyzers() ([]*analysis.Analyzer, error) {
	return []*analysis.Analyzer{Analyzer}, nil
}

func (plugin) GetLoadMode() string {
	return register.LoadModeTypesInfo
}

type functionInfo struct {
	decl      *ast.FuncDecl
	fn        *types.Func
	publishes bool
}

type cleanupMethod struct {
	fn   *types.Func
	info *functionInfo
}

type lifecycleOwner struct {
	cleanups []cleanupMethod
}

type lifecycleStart struct {
	start        *functionInfo
	ownerResults map[int]bool
	owners       []lifecycleOwner
}

func run(pass *analysis.Pass) (any, error) {
	parents := parentMap(pass.Files)
	functions := collectFunctions(pass)
	propagatePublishers(pass, functions)
	lifecycle := collectLifecycleStarts(functions)

	for _, info := range functions {
		checkPublishBeforeFailure(pass, info, functions)
		checkLifecycleStartCalls(pass, info, lifecycle, parents)
		checkIgnoredLifecycleCalls(pass, info, functions, parents)
	}
	checkGoroutineOwnership(pass, lifecycle)

	return nil, nil
}

func collectFunctions(pass *analysis.Pass) map[*types.Func]*functionInfo {
	out := make(map[*types.Func]*functionInfo)
	for _, file := range pass.Files {
		for _, decl := range file.Decls {
			fnDecl, ok := decl.(*ast.FuncDecl)
			if !ok || fnDecl.Body == nil {
				continue
			}
			fn, _ := pass.TypesInfo.Defs[fnDecl.Name].(*types.Func)
			if fn == nil {
				continue
			}
			out[fn] = &functionInfo{decl: fnDecl, fn: fn}
		}
	}
	return out
}

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

func assumeCallReturns(*ast.CallExpr) bool { return true }

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
			// A Serve call that is itself the returned error is a runtime loop,
			// not an earlier publication followed by a later startup failure.
			if ret, ok := node.(*ast.ReturnStmt); ok && published && !committed && returnMayFail(info.fn, ret) {
				pass.Reportf(info.decl.Name.Pos(),
					"[SLC100] %s can publish serving before a later error return; bind/acquire every fallible startup resource before launching Serve",
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

func collectLifecycleStarts(functions map[*types.Func]*functionInfo) map[*types.Func]*lifecycleStart {
	out := make(map[*types.Func]*lifecycleStart)
	for _, info := range functions {
		sig, _ := info.fn.Type().(*types.Signature)
		if sig == nil || sig.Recv() == nil || !isStartMethodName(info.fn.Name()) {
			continue
		}
		relation := &lifecycleStart{start: info, ownerResults: make(map[int]bool)}
		for i := range sig.Results().Len() {
			methods := cleanupMethods(sig.Results().At(i).Type(), functions)
			if len(methods) == 0 {
				continue
			}
			relation.ownerResults[i] = true
			relation.owners = append(relation.owners, lifecycleOwner{cleanups: methods})
		}
		if len(relation.owners) == 0 {
			methods := cleanupMethods(sig.Recv().Type(), functions)
			if len(methods) > 0 {
				relation.owners = append(relation.owners, lifecycleOwner{cleanups: methods})
			}
		}
		if len(relation.owners) > 0 {
			out[info.fn] = relation
		}
	}
	return out
}

func cleanupMethods(owner types.Type, functions map[*types.Func]*functionInfo) []cleanupMethod {
	sets := []*types.MethodSet{types.NewMethodSet(owner)}
	if _, pointer := types.Unalias(owner).(*types.Pointer); !pointer {
		if named := namedType(owner); named != nil {
			sets = append(sets, types.NewMethodSet(types.NewPointer(named)))
		}
	}
	var out []cleanupMethod
	for _, set := range sets {
		for method := range set.Methods() {
			fn, _ := method.Obj().(*types.Func)
			if fn == nil || !isCleanupMethodName(fn.Name()) {
				continue
			}
			out = append(out, cleanupMethod{fn: fn, info: functions[fn]})
		}
	}
	return uniqueCleanupMethods(out)
}

func uniqueCleanupMethods(methods []cleanupMethod) []cleanupMethod {
	seen := make(map[*types.Func]bool)
	out := make([]cleanupMethod, 0, len(methods))
	for _, method := range methods {
		if seen[method.fn] {
			continue
		}
		seen[method.fn] = true
		out = append(out, method)
	}
	return out
}

func checkLifecycleStartCalls(pass *analysis.Pass, info *functionInfo, lifecycle map[*types.Func]*lifecycleStart, parents map[ast.Node]ast.Node) {
	constructor := strings.HasPrefix(info.fn.Name(), "new") && len(info.fn.Name()) > len("new")
	ast.Inspect(info.decl.Body, func(node ast.Node) bool {
		if node == nil {
			return false
		}
		if _, ok := node.(*ast.FuncLit); ok {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn := calledFunction(pass, call)
		relation := lifecycle[fn]
		if relation == nil {
			return true
		}
		if constructor {
			pass.Reportf(call.Pos(),
				"[SLC101] constructor %s starts lifecycle-owned state; construct it here and start it from the owning Start phase so rollback can stop it",
				info.fn.Name())
			return true
		}
		if len(relation.ownerResults) > 0 && discardedLifecycleOwner(call, relation.ownerResults, parents) {
			pass.Reportf(call.Pos(),
				"[SLC101] discarded lifecycle owner returned by %s; retain it so cleanup can stop the started generation",
				fn.Name())
		}
		return true
	})
}

func discardedLifecycleOwner(call *ast.CallExpr, ownerResults map[int]bool, parents map[ast.Node]ast.Node) bool {
	parent := enclosingExpressionParent(call, parents)
	switch p := parent.(type) {
	case *ast.ExprStmt, *ast.GoStmt, *ast.DeferStmt:
		return true
	case *ast.AssignStmt:
		return assignmentDiscardsOwner(call, p, ownerResults)
	default:
		return false
	}
}

func enclosingExpressionParent(expr ast.Expr, parents map[ast.Node]ast.Node) ast.Node {
	parent := parents[expr]
	for {
		paren, ok := parent.(*ast.ParenExpr)
		if !ok {
			return parent
		}
		parent = parents[paren]
	}
}

func assignmentDiscardsOwner(call *ast.CallExpr, assign *ast.AssignStmt, ownerResults map[int]bool) bool {
	if len(assign.Rhs) == 1 && assign.Rhs[0] == call {
		for result := range ownerResults {
			if result >= len(assign.Lhs) || isBlankIdent(assign.Lhs[result]) {
				return true
			}
		}
		return false
	}
	for i, rhs := range assign.Rhs {
		if rhs == call {
			return ownerResults[0] && (i >= len(assign.Lhs) || isBlankIdent(assign.Lhs[i]))
		}
	}
	return false
}

func isBlankIdent(expr ast.Expr) bool {
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == "_"
}

func checkIgnoredLifecycleCalls(pass *analysis.Pass, info *functionInfo, functions map[*types.Func]*functionInfo, parents map[ast.Node]ast.Node) {
	lifecycleFn := isLifecycleFunction(info.fn.Name())
	ast.Inspect(info.decl.Body, func(node ast.Node) bool {
		if node == nil {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn := calledFunction(pass, call)
		if fn == nil || !callIgnored(call, parents) {
			return true
		}
		if isServeFunction(fn) || localPublisherReturnsError(fn, functions) {
			pass.Reportf(call.Pos(), "[SLC102] ignored %s error can leave a bound-but-dead serving endpoint; observe and retire unexpected Serve exits", fn.Name())
		}
		if lifecycleFn && isCleanupFunction(fn) && functionReturnsError(fn) {
			pass.Reportf(call.Pos(), "[SLC104] lifecycle cleanup discards %s error; propagate or join cleanup failures", fn.Name())
		}
		return true
	})
}

func localPublisherReturnsError(fn *types.Func, functions map[*types.Func]*functionInfo) bool {
	info := functions[fn]
	return info != nil && info.publishes && functionReturnsError(fn)
}

func isLifecycleFunction(name string) bool {
	lower := strings.ToLower(name)
	return isStartMethodName(name) || isCleanupMethodName(name) ||
		strings.HasPrefix(lower, "unwind") || strings.HasPrefix(lower, "rollback")
}

func isCleanupFunction(fn *types.Func) bool {
	return isCleanupMethodName(fn.Name())
}

func isStartMethodName(name string) bool {
	return name == methodStart || name == methodStartAPI
}

func isCleanupMethodName(name string) bool {
	switch name {
	case methodStop, methodStopRun, methodShutdownLocal, methodShutdown, methodClose:
		return true
	default:
		return false
	}
}

//nolint:gocyclo // Go has several syntax forms that intentionally discard call results.
func callIgnored(call *ast.CallExpr, parents map[ast.Node]ast.Node) bool {
	parent := parents[call]
	switch p := parent.(type) {
	case *ast.ExprStmt:
		return p.X == call
	case *ast.GoStmt:
		return p.Call == call
	case *ast.DeferStmt:
		return p.Call == call
	case *ast.AssignStmt:
		if len(p.Rhs) != 1 || p.Rhs[0] != call {
			return false
		}
		for _, lhs := range p.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok || id.Name != "_" {
				return false
			}
		}
		return len(p.Lhs) > 0
	}
	return false
}

func checkGoroutineOwnership(pass *analysis.Pass, lifecycle map[*types.Func]*lifecycleStart) {
	for _, relation := range lifecycle {
		goCount := countLifecycleLaunches(pass, relation.start.decl.Body)
		if goCount == 0 {
			continue
		}
		for _, owner := range relation.owners {
			cleanupName, bestWait, complete := bestCleanupEvidence(pass, owner.cleanups, goCount)
			if complete || !hasLocalCleanup(owner.cleanups) {
				continue
			}
			pass.Reportf(relation.start.decl.Name.Pos(),
				"[SLC103] %s launches %d lifecycle goroutine(s) but %s visibly waits for only %d completion signal(s); cleanup may return while owned goroutines still run",
				relation.start.fn.Name(), goCount, cleanupName, bestWait)
		}
	}
}

func bestCleanupEvidence(pass *analysis.Pass, cleanups []cleanupMethod, goCount int) (string, int, bool) {
	bestName := "cleanup"
	bestWait := 0
	for _, cleanup := range cleanups {
		if cleanup.info == nil || cleanup.info.decl.Body == nil {
			continue
		}
		evidence := cleanupEvidence(pass, cleanup.info.decl.Body)
		if evidence.waitGroup || evidence.receives >= goCount {
			return cleanup.fn.Name(), evidence.receives, true
		}
		if evidence.receives >= bestWait {
			bestName = cleanup.fn.Name()
			bestWait = evidence.receives
		}
	}
	return bestName, bestWait, false
}

func hasLocalCleanup(cleanups []cleanupMethod) bool {
	for _, cleanup := range cleanups {
		if cleanup.info != nil && cleanup.info.decl.Body != nil {
			return true
		}
	}
	return false
}

func countLifecycleLaunches(pass *analysis.Pass, body *ast.BlockStmt) int {
	count := 0
	ast.Inspect(body, func(node ast.Node) bool {
		if node == nil {
			return false
		}
		if _, ok := node.(*ast.FuncLit); ok {
			return false
		}
		if _, ok := node.(*ast.GoStmt); ok {
			count++
		}
		if call, ok := node.(*ast.CallExpr); ok && isSyncMethodCall(pass, call, "WaitGroup", "Go") {
			count++
		}
		return true
	})
	return count
}

type waitEvidence struct {
	receives  int
	waitGroup bool
}

func cleanupEvidence(pass *analysis.Pass, body *ast.BlockStmt) waitEvidence {
	var evidence waitEvidence
	var inspect func(ast.Node)
	inspect = func(root ast.Node) {
		ast.Inspect(root, func(node ast.Node) bool {
			if node == nil {
				return false
			}
			if _, ok := node.(*ast.FuncLit); ok {
				return false
			}
			if unary, ok := node.(*ast.UnaryExpr); ok && unary.Op == token.ARROW {
				evidence.receives++
			}
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if isSyncMethodCall(pass, call, "WaitGroup", "Wait") {
				evidence.waitGroup = true
			}
			if isSyncMethodCall(pass, call, "Once", "Do") {
				for _, arg := range call.Args {
					if lit, ok := arg.(*ast.FuncLit); ok {
						inspect(lit.Body)
					}
				}
			}
			return true
		})
	}
	inspect(body)
	return evidence
}

func isSyncMethodCall(pass *analysis.Pass, call *ast.CallExpr, owner, method string) bool {
	fn := calledFunction(pass, call)
	if fn == nil || fn.Name() != method {
		return false
	}
	sig, _ := fn.Type().(*types.Signature)
	if sig == nil || sig.Recv() == nil {
		return false
	}
	named := namedType(sig.Recv().Type())
	return named != nil && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "sync" && named.Obj().Name() == owner
}

func parentMap(files []*ast.File) map[ast.Node]ast.Node {
	parents := make(map[ast.Node]ast.Node)
	for _, file := range files {
		var stack []ast.Node
		ast.Inspect(file, func(node ast.Node) bool {
			if node == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			if len(stack) > 0 {
				parents[node] = stack[len(stack)-1]
			}
			stack = append(stack, node)
			return true
		})
	}
	return parents
}

func calledFunction(pass *analysis.Pass, call *ast.CallExpr) *types.Func {
	var obj types.Object
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		obj = pass.TypesInfo.Uses[fun]
	case *ast.SelectorExpr:
		obj = pass.TypesInfo.Uses[fun.Sel]
	}
	fn, _ := obj.(*types.Func)
	return fn
}

func namedType(t types.Type) *types.Named {
	for {
		t = types.Unalias(t)
		pointer, ok := t.(*types.Pointer)
		if !ok {
			break
		}
		t = pointer.Elem()
	}
	named, _ := t.(*types.Named)
	return named
}

func functionReturnsError(fn *types.Func) bool {
	sig, _ := fn.Type().(*types.Signature)
	return sig != nil && signatureReturnsError(sig)
}

func signatureReturnsError(sig *types.Signature) bool {
	errType := types.Universe.Lookup("error").Type()
	for result := range sig.Results().Variables() {
		if types.Identical(result.Type(), errType) {
			return true
		}
	}
	return false
}
