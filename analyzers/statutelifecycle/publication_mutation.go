package statutelifecycle

import (
	"go/ast"
	"go/token"
	"go/types"
	"maps"
	"strings"
)

// Publication alone can bind a direct address argument to a known local body.
// Generic pathResolver and the independent join analysis remain conservative.
func (w *publicationWalker) pathResolver(body *ast.BlockStmt) *pathResolver {
	bounded := make(map[ast.Expr]bool)
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || w.functions[calledFunction(w.pass, call)] == nil {
			return true
		}
		for _, arg := range call.Args {
			if ptr, ok := ast.Unparen(arg).(*ast.UnaryExpr); ok && ptr.Op == token.AND {
				bounded[ptr] = true
			}
		}
		return true
	})
	r := &pathResolver{
		pass: w.pass, aliases: make(map[*types.Var]aliasTarget), aliasRHS: bounded,
		addrAliases: make(map[*types.Var]bool), written: make(map[*types.Var][]string), mutated: make(map[*types.Var]bool),
	}
	ast.Inspect(body, func(node ast.Node) bool {
		if expr, ok := node.(ast.Expr); ok && bounded[expr] {
			return true
		}
		recordNodeWrites(node, func(id *ast.Ident) {
			if v, _ := w.pass.TypesInfo.Uses[id].(*types.Var); v != nil {
				r.mutated[v] = true
			}
		})
		return true
	})
	r.collectAliases(body)
	w.channelAliases(r, body)
	r.collectWrittenPaths(body)
	r.collectAliasEscapes(body)
	return r
}

//nolint:gocyclo // channel aliases extend this proof without changing the generic pointer resolver.
func (w *publicationWalker) channelAliases(r *pathResolver, body *ast.BlockStmt) {
	ast.Inspect(body, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok || assign.Tok != token.DEFINE || len(assign.Lhs) != len(assign.Rhs) {
			return true
		}
		for i, lhs := range assign.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok {
				continue
			}
			v, _ := w.pass.TypesInfo.Defs[id].(*types.Var)
			if v == nil || r.mutated[v] {
				continue
			}
			if _, channel := w.pass.TypesInfo.TypeOf(assign.Rhs[i]).Underlying().(*types.Chan); !channel {
				continue
			}
			if root, path, ok := r.resolveExpr(assign.Rhs[i]); ok {
				r.aliases[v] = aliasTarget{root: root, path: path}
			}
		}
		return true
	})
}

func (w *publicationWalker) trackCleanup() {
	for key := range w.stops {
		w.tracked[key] = true
	}
	for key := range w.waits {
		w.tracked[key] = true
	}
}

func (w *publicationWalker) relevant(key groupKey) bool {
	for tracked := range w.tracked {
		if key.root != nil && key.root == tracked.root &&
			(key.path == tracked.path || strings.HasPrefix(tracked.path, key.path+".") || strings.HasPrefix(key.path, tracked.path+".")) {
			return true
		}
	}
	return false
}

// Inspect mutation-only helpers too: a non-publishing call can replace the
// storage rollback later reads. Only calls bound to the tracked owner matter.
func (w *publicationWalker) mutationBody(body *ast.BlockStmt, incoming publicationBindings) {
	bindings := w.localBindings(body, incoming)
	ast.Inspect(body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.FuncLit:
			return false
		case *ast.CallExpr:
			w.mutationCall(n, bindings)
			return false
		case *ast.AssignStmt:
			w.assignmentEscapes(n, bindings)
		case *ast.ReturnStmt:
			for _, result := range n.Results {
				w.escape(result, bindings)
			}
		case *ast.CompositeLit:
			for _, element := range n.Elts {
				if pair, ok := element.(*ast.KeyValueExpr); ok {
					w.escape(pair.Value, bindings)
				} else {
					w.escape(element, bindings)
				}
			}
		}
		return true
	})
}

//nolint:gocyclo // storage, local aliases and certified owner transfers have different escape semantics.
func (w *publicationWalker) assignmentEscapes(assign *ast.AssignStmt, bindings publicationBindings) {
	if len(assign.Lhs) != len(assign.Rhs) {
		return
	}
	for i, rhs := range assign.Rhs {
		w.opaqueCaptures(rhs, bindings)
		if id, ok := assign.Lhs[i].(*ast.Ident); ok {
			v, _ := w.pass.TypesInfo.Defs[id].(*types.Var)
			if v == nil {
				v, _ = w.pass.TypesInfo.Uses[id].(*types.Var)
			}
			if v != nil && v.Parent() != w.pass.Pkg.Scope() {
				continue // A local alias does not itself escape.
			}
		}
		left, leftOK := w.resolveRaw(assign.Lhs[i], bindings)
		right, rightOK := w.resolveRaw(rhs, bindings)
		if leftOK && rightOK && left == right {
			continue // The already-certified initial ownership transfer.
		}
		w.escape(rhs, bindings)
	}
}

func (w *publicationWalker) opaqueCaptures(expr ast.Expr, bindings publicationBindings) {
	ast.Inspect(expr, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.CallExpr:
			return false // Evaluated effects are traversed separately.
		case *ast.FuncLit:
			w.captureEscapes(n, bindings)
			return false
		case *ast.SelectorExpr:
			selection := w.pass.TypesInfo.Selections[n]
			if selection != nil && selection.Kind() == types.MethodVal {
				key, ok := w.methodReceiver(n, bindings)
				if !ok {
					// Cleanup cannot use an unsupported receiver copy, but its
					// captured references still escape through the method value.
					key, ok = w.resolveRaw(n.X, bindings)
				}
				if ok && w.relevant(key) {
					w.invalid[key] = true
				}
			}
		}
		return true
	})
}

func (w *publicationWalker) captureEscapes(lit *ast.FuncLit, bindings publicationBindings) {
	ast.Inspect(lit.Body, func(node ast.Node) bool {
		id, ok := node.(*ast.Ident)
		if !ok {
			return true
		}
		if key, ok := w.resolveRaw(id, bindings); ok && w.relevant(key) {
			w.invalid[key] = true
		}
		return true
	})
}

func (w *publicationWalker) escape(expr ast.Expr, bindings publicationBindings) {
	ast.Inspect(expr, func(node ast.Node) bool {
		if _, call := node.(*ast.CallExpr); call {
			return false // Call effects are modeled separately from the returned value.
		}
		value, ok := node.(ast.Expr)
		if !ok {
			return true
		}
		key, resolved := w.resolveRaw(value, bindings)
		if !resolved {
			return true
		}
		t := w.pass.TypesInfo.TypeOf(value)
		_, channel := t.Underlying().(*types.Chan)
		aggregate := false
		switch t.Underlying().(type) {
		case *types.Struct, *types.Array:
			aggregate = true // A value aggregate may retain shared reference fields.
		}
		if (isPointerType(t) || channel || aggregate || !publicationCopySafe(t)) && w.relevant(key) {
			w.invalid[key] = true
		}
		return false // A scalar projection doesn't leak its owner's storage.
	})
}

//nolint:gocyclo // primitive summaries, local bindings and unknown escapes must remain distinct.
func (w *publicationWalker) mutationCall(call *ast.CallExpr, bindings publicationBindings) {
	// Argument calls execute even when the outer call has a primitive summary.
	evaluatedCalls(call, func(nested *ast.CallExpr) { w.mutationCall(nested, bindings) })
	if lit, ok := ast.Unparen(call.Fun).(*ast.FuncLit); ok {
		w.mutationBody(lit.Body, w.literalBindings(lit, call.Args, bindings))
		return
	}
	fn := calledFunction(w.pass, call)
	if fn != nil {
		sig, _ := fn.Type().(*types.Signature)
		if sig.Recv() != nil {
			sel, _ := ast.Unparen(call.Fun).(*ast.SelectorExpr)
			if _, ok := w.methodReceiver(sel, bindings); !ok {
				w.unknownMutationCall(call, bindings)
				return
			}
		}
	}
	if id, ok := ast.Unparen(call.Fun).(*ast.Ident); ok {
		if builtin, ok := w.pass.TypesInfo.Uses[id].(*types.Builtin); ok && builtin.Name() == builtinCloseName {
			if !w.completion[call] {
				for _, arg := range call.Args {
					w.escape(arg, bindings)
				}
			}
			return
		}
	}
	if fn != nil && (isServeFunction(fn) || isServerStopFunction(fn)) {
		return
	}
	if isSyncMethodCall(w.pass, call, "WaitGroup", "Wait") || isSyncMethodCall(w.pass, call, "WaitGroup", "Add") {
		// Counter changes do not replace or escape the group storage. Operand
		// effects and the exact receiver guard above still apply to Add.
		return
	}
	if isSyncMethodCall(w.pass, call, "WaitGroup", "Done") {
		if !w.completion[call] {
			if sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr); ok {
				if key, ok := w.methodReceiver(sel, bindings); ok && w.relevant(key) {
					w.invalid[key] = true
				}
			}
		}
		return
	}
	if isSyncMethodCall(w.pass, call, "Once", "Do") || isSyncMethodCall(w.pass, call, "WaitGroup", "Go") {
		for _, arg := range call.Args {
			if lit, ok := ast.Unparen(arg).(*ast.FuncLit); ok {
				w.mutationBody(lit.Body, bindings)
			}
		}
		return
	}
	info := w.functions[fn]
	if info == nil {
		w.unknownMutationCall(call, bindings)
		return
	}
	for _, arg := range call.Args {
		if _, ok := w.resolveRaw(arg, bindings); !ok {
			// Unknown aggregate/callback representations cannot be bound as
			// scalar storage identities, even when the callee is local.
			w.escape(arg, bindings)
			w.opaqueCaptures(arg, bindings)
		}
	}
	next := make(publicationBindings)
	sig, _ := fn.Type().(*types.Signature)
	if sig.Recv() != nil {
		sel, _ := ast.Unparen(call.Fun).(*ast.SelectorExpr)
		next[receiverVar(w.pass, info.decl)], _ = w.methodReceiver(sel, bindings)
	}
	for i := 0; i < sig.Params().Len() && i < len(call.Args); i++ {
		if sig.Variadic() && i == sig.Params().Len()-1 {
			for _, arg := range call.Args[i:] {
				w.escape(arg, bindings)
			}
			break // Never equate a variadic aggregate with its first element.
		}
		next[sig.Params().At(i)] = w.argumentIdentity(call.Args[i], sig.Params().At(i).Type(), bindings)
	}
	relevant := false
	for _, key := range next {
		relevant = relevant || w.relevant(key)
	}
	if !relevant {
		return
	}
	if w.active[fn] {
		for _, key := range next {
			if w.relevant(key) {
				w.invalid[key] = true
			}
		}
		return
	}
	w.active[fn] = true
	w.mutationBody(info.decl.Body, next)
	delete(w.active, fn)
}

// An unknown receiver or callee cannot justify parameter rebasing. Actual
// pointer/aggregate arguments and captures retain conservative escape rules.
func (w *publicationWalker) unknownMutationCall(call *ast.CallExpr, bindings publicationBindings) {
	for _, arg := range call.Args {
		w.escape(arg, bindings)
		w.opaqueCaptures(arg, bindings)
		if lit, ok := ast.Unparen(arg).(*ast.FuncLit); ok {
			w.mutationBody(lit.Body, bindings)
		}
	}
	if sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr); ok {
		w.escape(sel.X, bindings)
	}
}

// Function receivers and arguments execute before the called body; inert
// function-literal bodies do not. Visit each evaluated nested call once.
func evaluatedCalls(call *ast.CallExpr, visit func(*ast.CallExpr)) {
	inspect := func(expr ast.Expr) {
		ast.Inspect(expr, func(node ast.Node) bool {
			if _, ok := node.(*ast.FuncLit); ok {
				return false
			}
			if nested, ok := node.(*ast.CallExpr); ok {
				visit(nested)
				return false
			}
			return true
		})
	}
	inspect(call.Fun)
	for _, arg := range call.Args {
		inspect(arg)
	}
}

func (w *publicationWalker) literalBindings(lit *ast.FuncLit, args []ast.Expr, incoming publicationBindings) publicationBindings {
	bindings := make(publicationBindings)
	maps.Copy(bindings, incoming)
	sig, _ := w.pass.TypesInfo.TypeOf(lit).(*types.Signature)
	for i := 0; i < sig.Params().Len() && i < len(args); i++ {
		if sig.Variadic() && i == sig.Params().Len()-1 {
			for _, arg := range args[i:] {
				w.escape(arg, incoming)
			}
			bindings[sig.Params().At(i)] = groupKey{}
			break
		}
		bindings[sig.Params().At(i)] = w.argumentIdentity(args[i], sig.Params().At(i).Type(), incoming)
	}
	return bindings
}
