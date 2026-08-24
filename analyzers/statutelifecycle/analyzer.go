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
	pluginName     = "statutelifecycle"
	methodStart    = "start"
	methodStartAPI = "Start"
	methodStop     = "stop"
	methodShutdown = "Shutdown"
	methodClose    = "Close"
)

// Analyzer checks Statute-specific lifecycle ownership invariants.
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

func run(pass *analysis.Pass) (any, error) {
	parents := parentMap(pass.Files)
	functions := collectFunctions(pass)
	propagatePublishers(pass, functions)

	for _, info := range functions {
		checkPublishBeforeFailure(pass, info, functions)
		checkConstructorStartsLifecycle(pass, info)
		checkIgnoredLifecycleCalls(pass, info, functions, parents)
	}
	checkGoroutineOwnership(pass, functions)

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
			if info.publishes || !containsPublisher(pass, info.decl.Body, functions) {
				continue
			}
			info.publishes = true
			changed = true
		}
	}
}

//nolint:gocyclo // publisher propagation deliberately distinguishes calls, helpers, and launched closures.
func containsPublisher(pass *analysis.Pass, root ast.Node, functions map[*types.Func]*functionInfo) bool {
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
				found = true
				return false
			}
			if lit, ok := n.Call.Fun.(*ast.FuncLit); ok && containsPublisher(pass, lit.Body, functions) {
				found = true
			}
			return false
		case *ast.CallExpr:
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
	named := namedType(sig.Recv().Type())
	if named == nil || named.Obj().Pkg() == nil {
		return false
	}
	pkg := named.Obj().Pkg().Path()
	name := named.Obj().Name()
	return (pkg == "net/http" && name == "Server") ||
		(pkg == "github.com/quic-go/quic-go/http3" && name == "Server")
}

//nolint:gocyclo // CFG state traversal is clearer as one publication/commit state machine.
func checkPublishBeforeFailure(pass *analysis.Pass, info *functionInfo, functions map[*types.Func]*functionInfo) {
	if !isStartupFunction(info.fn.Name()) || !functionReturnsError(info.fn) || info.decl.Body == nil {
		return
	}
	graph := cfg.New(info.decl.Body, nil)
	if len(graph.Blocks) == 0 {
		return
	}

	type state struct {
		block     *cfg.Block
		published bool
		committed bool
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
			if !committed && nodePublishes(pass, node, functions) {
				published = true
			}
		}

		for _, succ := range current.block.Succs {
			queue = append(queue, state{block: succ, published: published, committed: committed})
		}
	}
}

func isStartupFunction(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, methodStart) || strings.HasPrefix(lower, "bind")
}

func nodePublishes(pass *analysis.Pass, node ast.Node, functions map[*types.Func]*functionInfo) bool {
	return containsPublisher(pass, node, functions)
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

//nolint:gocyclo // type-aware lifecycle matching is intentionally conservative and explicit.
func checkConstructorStartsLifecycle(pass *analysis.Pass, info *functionInfo) {
	if !strings.HasPrefix(info.fn.Name(), "new") || len(info.fn.Name()) == len("new") {
		return
	}
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
		if fn == nil || (fn.Name() != methodStart && fn.Name() != methodStartAPI) {
			return true
		}
		sig, _ := fn.Type().(*types.Signature)
		if sig == nil || sig.Recv() == nil || !hasStopMethod(sig.Recv().Type()) {
			return true
		}
		pass.Reportf(call.Pos(),
			"[SLC101] constructor %s starts lifecycle-owned state; construct it here and start it from the owning Start phase so rollback can stop it",
			info.fn.Name())
		return true
	})
}

func hasStopMethod(recv types.Type) bool {
	set := types.NewMethodSet(recv)
	for method := range set.Methods() {
		name := method.Obj().Name()
		if name == methodStop || name == methodShutdown || name == methodClose {
			return true
		}
	}
	return false
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
	return lower == methodStart || lower == strings.ToLower(methodShutdown) || lower == methodStop ||
		strings.HasPrefix(lower, "unwind") || strings.HasPrefix(lower, "rollback")
}

func isCleanupFunction(fn *types.Func) bool {
	switch fn.Name() {
	case methodClose, methodShutdown:
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

//nolint:gocyclo // paired start/stop accounting is easiest to audit in one place.
func checkGoroutineOwnership(pass *analysis.Pass, functions map[*types.Func]*functionInfo) {
	type methods struct {
		start *functionInfo
		stop  *functionInfo
	}
	byRecv := make(map[*types.TypeName]*methods)
	for _, info := range functions {
		sig, _ := info.fn.Type().(*types.Signature)
		if sig == nil || sig.Recv() == nil {
			continue
		}
		named := namedType(sig.Recv().Type())
		if named == nil {
			continue
		}
		entry := byRecv[named.Obj()]
		if entry == nil {
			entry = &methods{}
			byRecv[named.Obj()] = entry
		}
		switch info.fn.Name() {
		case methodStart, methodStartAPI:
			entry.start = info
		case methodStop, methodShutdown:
			entry.stop = info
		}
	}

	for _, pair := range byRecv {
		if pair.start == nil || pair.stop == nil {
			continue
		}
		goCount := countGoStatements(pair.start.decl.Body)
		if goCount == 0 || hasWaitGroupWait(pass, pair.stop.decl.Body) {
			continue
		}
		waitCount := countReceives(pair.stop.decl.Body)
		if waitCount >= goCount {
			continue
		}
		pass.Reportf(pair.start.decl.Name.Pos(),
			"[SLC103] %s launches %d lifecycle goroutine(s) but %s visibly waits for only %d completion signal(s); stop may return while owned goroutines still run",
			pair.start.fn.Name(), goCount, pair.stop.fn.Name(), waitCount)
	}
}

func countGoStatements(body *ast.BlockStmt) int {
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
		return true
	})
	return count
}

func countReceives(body *ast.BlockStmt) int {
	count := 0
	ast.Inspect(body, func(node ast.Node) bool {
		if node == nil {
			return false
		}
		if _, ok := node.(*ast.FuncLit); ok {
			return false
		}
		if unary, ok := node.(*ast.UnaryExpr); ok && unary.Op == token.ARROW {
			count++
		}
		return true
	})
	return count
}

//nolint:gocyclo // receiver/type checks keep arbitrary Wait methods from silencing lifecycle findings.
func hasWaitGroupWait(pass *analysis.Pass, body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(node ast.Node) bool {
		if node == nil || found {
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
		if fn == nil || fn.Name() != "Wait" {
			return true
		}
		sig, _ := fn.Type().(*types.Signature)
		if sig == nil || sig.Recv() == nil {
			return true
		}
		named := namedType(sig.Recv().Type())
		if named != nil && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "sync" && named.Obj().Name() == "WaitGroup" {
			found = true
		}
		return true
	})
	return found
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
	t = types.Unalias(t)
	if pointer, ok := t.(*types.Pointer); ok {
		t = types.Unalias(pointer.Elem())
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
