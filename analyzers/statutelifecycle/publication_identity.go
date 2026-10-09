package statutelifecycle

import (
	"go/ast"
	"go/token"
	"go/types"
	"maps"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// Publication identity is deliberately separate from SLC103's join obligations.
// Each proof binds concrete receivers and arguments at the caller, never a type.
type publicationResource struct {
	server  groupKey
	signals map[groupKey]bool
}

type publicationBindings map[*types.Var]groupKey

type publicationWalker struct {
	pass       *analysis.Pass
	functions  map[*types.Func]*functionInfo
	active     map[*types.Func]bool
	resolved   bool
	stops      map[groupKey]bool
	waits      map[groupKey]bool
	published  []publicationResource
	rollback   bool
	invalid    map[groupKey]bool
	tracked    map[groupKey]bool
	completion map[*ast.CallExpr]bool
}

func rollbackResources(pass *analysis.Pass, v *types.Var, functions map[*types.Func]*functionInfo) (map[groupKey]bool, map[groupKey]bool) {
	w := newPublicationWalker(pass, functions)
	w.rollback = true
	fn := lookupMethod(v.Type(), "rollback")
	info := functions[fn]
	if info != nil {
		w.function(fn, publicationBindings{receiverVar(pass, info.decl): {root: v}}, nil)
		w.trackCleanup()
		w.mutationBody(info.decl.Body, publicationBindings{receiverVar(pass, info.decl): {root: v}})
	}
	for key := range w.stops {
		if w.invalidated(key) {
			delete(w.stops, key)
		}
	}
	for key := range w.waits {
		if w.invalidated(key) {
			delete(w.waits, key)
		}
	}
	return w.stops, w.waits
}

func publicationResources(pass *analysis.Pass, call *ast.CallExpr, functions map[*types.Func]*functionInfo, body *ast.BlockStmt) ([]publicationResource, bool) {
	w := newPublicationWalker(pass, functions)
	bindings := w.localBindings(body, nil)
	w.call(call, bindings, nil)
	for _, resource := range w.published {
		w.tracked[resource.server] = true
		for signal := range resource.signals {
			w.tracked[signal] = true
		}
	}
	w.mutationBody(body, bindings)
	for _, resource := range w.published {
		w.resolved = w.resolved && !w.invalidated(resource.server)
		for signal := range resource.signals {
			if w.invalidated(signal) {
				delete(resource.signals, signal)
			}
		}
	}
	return w.published, w.resolved
}

func newPublicationWalker(pass *analysis.Pass, functions map[*types.Func]*functionInfo) *publicationWalker {
	return &publicationWalker{
		pass: pass, functions: functions, active: make(map[*types.Func]bool), resolved: true,
		stops: make(map[groupKey]bool), waits: make(map[groupKey]bool), invalid: make(map[groupKey]bool), tracked: make(map[groupKey]bool), completion: make(map[*ast.CallExpr]bool),
	}
}

func (w *publicationWalker) invalidated(key groupKey) bool {
	for prefix := range w.invalid {
		if prefix.root == key.root && (prefix.path == key.path || strings.HasPrefix(key.path, prefix.path+".")) {
			return true
		}
	}
	return false
}

// resolve binds the complete field path, preserving pointer aliases. Mutated
// roots and unsupported expressions have no identity and cannot excuse serving.
func (w *publicationWalker) resolve(expr ast.Expr, bindings publicationBindings) (groupKey, bool) {
	key, ok := w.resolveRaw(expr, bindings)
	return key, ok && !w.invalidated(key)
}

//nolint:gocyclo // exact identity resolution handles each supported AST and binding form.
func (w *publicationWalker) resolveRaw(expr ast.Expr, bindings publicationBindings) (groupKey, bool) {
	switch e := ast.Unparen(expr).(type) {
	case *ast.Ident:
		v, _ := w.pass.TypesInfo.Uses[e].(*types.Var)
		if v == nil {
			v, _ = w.pass.TypesInfo.Defs[e].(*types.Var)
		}
		if v == nil {
			return groupKey{}, false
		}
		if key, ok := bindings[v]; ok {
			return key, key.root != nil
		}
		return groupKey{root: v}, true
	case *ast.SelectorExpr:
		selection := w.pass.TypesInfo.Selections[e]
		if selection == nil || selection.Kind() != types.FieldVal {
			return groupKey{}, false
		}
		key, ok := w.resolveRaw(e.X, bindings)
		path, valid := selectionFieldPath(selection)
		key.path += path
		return key, ok && valid
	case *ast.UnaryExpr:
		if e.Op == token.AND {
			return w.resolveRaw(e.X, bindings)
		}
	case *ast.StarExpr:
		return w.resolveRaw(e.X, bindings)
	}
	return groupKey{}, false
}

func (w *publicationWalker) function(fn *types.Func, bindings publicationBindings, signals map[groupKey]bool) {
	info := w.functions[fn]
	if info == nil || info.decl.Body == nil {
		return
	}
	if w.active[fn] {
		w.resolved = false
		return
	}
	w.active[fn] = true
	defer delete(w.active, fn)
	bindings = w.localBindings(info.decl.Body, bindings)
	w.body(info.decl.Body, bindings, signals)
}

// localBindings recognizes single-assignment aliases and the bounded ownership
// transfer used by early health startup: a fresh local handle stored once into
// the attempt before launch. Reassignment invalidates the alias; each sibling
// resource retains its own identity.
//
//nolint:gocyclo // bounded transfer certification keeps declaration, ordering, writes and aliases in one proof.
func (w *publicationWalker) localBindings(body *ast.BlockStmt, incoming publicationBindings) publicationBindings {
	bindings := make(publicationBindings)
	maps.Copy(bindings, incoming)
	resolver := w.pathResolver(body)
	for v := range resolver.mutated {
		if prior, ok := incoming[v]; ok && prior.root != nil {
			// A rebinding must not erase mutations through the old argument.
			w.invalid[prior] = true
		}
		bindings[v] = groupKey{}
	}
	for v, alias := range resolver.aliases {
		key := groupKey(alias)
		if bound, ok := bindings[key.root]; ok {
			key = groupKey{root: bound.root, path: bound.path + key.path}
		}
		bindings[v] = key
	}
	fresh := make(map[*types.Var]token.Pos)
	var transfers []*ast.AssignStmt
	firstPublication := body.End()
	ast.Inspect(body, func(node ast.Node) bool {
		if _, ok := node.(*ast.FuncLit); ok {
			return false
		}
		if goStmt, ok := node.(*ast.GoStmt); ok && goStmt.Pos() < firstPublication {
			firstPublication = goStmt.Pos()
		}
		if call, ok := node.(*ast.CallExpr); ok && callPublishes(w.pass, call, w.functions) && call.Pos() < firstPublication {
			firstPublication = call.Pos()
		}
		return true
	})
	// Only direct statements prove unconditional declaration/transfer ordering.
	for _, stmt := range body.List {
		assign, ok := stmt.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			continue
		}
		if id, ok := assign.Lhs[0].(*ast.Ident); ok && assign.Tok == token.DEFINE {
			if ptr, ok := assign.Rhs[0].(*ast.UnaryExpr); ok && ptr.Op == token.AND {
				if _, ok := ptr.X.(*ast.CompositeLit); ok {
					v, _ := w.pass.TypesInfo.Defs[id].(*types.Var)
					if !resolver.mutated[v] {
						fresh[v] = assign.Pos()
					}
				}
			}
		}
		if _, ok := assign.Lhs[0].(*ast.SelectorExpr); ok {
			transfers = append(transfers, assign)
		}
	}
	certified := make(map[groupKey]bool)
	for _, assign := range transfers {
		id, ok := assign.Rhs[0].(*ast.Ident)
		if !ok {
			continue
		}
		v, _ := w.pass.TypesInfo.Uses[id].(*types.Var)
		key, valid := w.resolveRaw(assign.Lhs[0], bindings)
		if !valid || fresh[v] == token.NoPos || fresh[v] >= assign.Pos() || assign.Pos() >= firstPublication {
			continue
		}
		root, path, valid := resolver.resolveExpr(assign.Lhs[0])
		writes := 0
		for _, written := range resolver.written[root] {
			if written == path {
				writes++
			}
		}
		if valid && writes == 1 {
			bindings[v] = key
			certified[groupKey{root: root, path: path}] = true
		}
	}
	for root, paths := range resolver.written {
		for _, path := range paths {
			if certified[groupKey{root: root, path: path}] {
				continue
			}
			key := groupKey{root: root, path: path}
			if bound, ok := bindings[root]; ok {
				key = groupKey{root: bound.root, path: bound.path + path}
			}
			w.invalid[key] = true
		}
	}
	return bindings
}

//nolint:gocyclo // deferred completion and launch frames must be distinguished during the AST walk.
func (w *publicationWalker) body(body *ast.BlockStmt, bindings publicationBindings, inherited map[groupKey]bool) {
	signals := make(map[groupKey]bool)
	for key := range inherited {
		signals[key] = true
	}
	// Only deferred completion in this execution frame joins its Serve calls.
	for _, stmt := range body.List {
		deferred, ok := stmt.(*ast.DeferStmt)
		if !ok {
			continue
		}
		call := deferred.Call
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == builtinCloseName && len(call.Args) == 1 {
			if _, builtin := w.pass.TypesInfo.Uses[id].(*types.Builtin); !builtin {
				continue
			}
			if key, ok := w.resolve(call.Args[0], bindings); ok {
				signals[key] = true
				w.completion[call] = true
			}
		}
		if isSyncMethodCall(w.pass, call, "WaitGroup", "Done") {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if key, ok := w.resolve(sel.X, bindings); ok {
					signals[key] = true
					w.completion[call] = true
				}
			}
		}
	}
	ast.Inspect(body, func(node ast.Node) bool {
		switch n := node.(type) {
		case nil, *ast.FuncLit:
			return false
		case *ast.UnaryExpr:
			if w.rollback && n.Op == token.ARROW {
				if key, ok := w.resolve(n.X, bindings); ok {
					w.waits[key] = true
				}
			}
		case *ast.GoStmt:
			if !w.rollback {
				w.call(n.Call, bindings, nil)
			}
			return false
		case *ast.CallExpr:
			w.call(n, bindings, signals)
			return false
		}
		return true
	})
}

//nolint:gocyclo // serving, stopping, joins and helper calls have deliberately separate provenance rules.
func (w *publicationWalker) call(call *ast.CallExpr, bindings publicationBindings, signals map[groupKey]bool) {
	// Go evaluates arguments before entering a helper. A local observer like
	// logServeExit(..., server.Serve(...)) must not hide that publication.
	evaluatedCalls(call, func(nested *ast.CallExpr) { w.call(nested, bindings, signals) })
	if lit, ok := ast.Unparen(call.Fun).(*ast.FuncLit); ok {
		w.body(lit.Body, w.literalBindings(lit, call.Args, bindings), signals)
		return
	}
	fn := calledFunction(w.pass, call)
	if fn == nil {
		return
	}
	sel, selected := call.Fun.(*ast.SelectorExpr)
	if selected && (isServeFunction(fn) || isServerStopFunction(fn) || isSyncMethodCall(w.pass, call, "WaitGroup", "Wait")) {
		key, ok := w.resolve(sel.X, bindings)
		if isServeFunction(fn) && !w.rollback {
			w.resolved = w.resolved && ok
			w.published = append(w.published, publicationResource{server: key, signals: signals})
		}
		if ok && w.rollback {
			if isServerStopFunction(fn) {
				w.stops[key] = true
			}
			if isSyncMethodCall(w.pass, call, "WaitGroup", "Wait") {
				w.waits[key] = true
			}
		}
		return
	}
	if isSyncMethodCall(w.pass, call, "Once", "Do") || (!w.rollback && isSyncMethodCall(w.pass, call, "WaitGroup", "Go")) {
		for _, arg := range call.Args {
			if lit, ok := arg.(*ast.FuncLit); ok {
				joined := signals
				if isSyncMethodCall(w.pass, call, "WaitGroup", "Go") {
					joined = make(map[groupKey]bool)
					if key, ok := w.resolve(sel.X, bindings); ok {
						joined[key] = true
					}
				}
				w.body(lit.Body, bindings, joined)
			}
		}
		return
	}
	info := w.functions[fn]
	if info == nil {
		return
	}
	next := make(publicationBindings)
	sig, _ := fn.Type().(*types.Signature)
	if sig.Recv() != nil {
		key, _ := w.resolve(sel.X, bindings)
		next[receiverVar(w.pass, info.decl)] = key
	}
	for i := 0; i < sig.Params().Len(); i++ {
		param := sig.Params().At(i)
		key := groupKey{}
		if i < len(call.Args) && (!sig.Variadic() || i != sig.Params().Len()-1) {
			key, _ = w.resolve(call.Args[i], bindings)
		}
		next[param] = key
	}
	w.function(fn, next, signals)
}
