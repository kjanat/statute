package statutelifecycle

// SLC103 models goroutine ownership as obligations against evidence. Every
// lifecycle launch in a start body creates one obligation: a raw go
// statement owes one completion signal, and a WaitGroup launch owes a Wait
// on the exact same group, normalized to lifecycle owner root plus the
// complete field-selection path. Cleanup evidence is collected per owner
// with the same normalization, so a Wait on one group can never discharge
// work launched through another group, a raw go, or another owner.
// Launches whose group cannot be attributed to a lifecycle owner are
// undischargeable and diagnosed: unknown provenance fails closed.

import (
	"go/ast"
	"go/token"
	"go/types"
	"sort"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// groupKey identifies one normalized WaitGroup: the root variable it is
// selected from and the complete field-selection path below that root.
type groupKey struct {
	root *types.Var
	path string
}

// aliasTarget is what a single-assignment local alias resolves to.
type aliasTarget struct {
	root *types.Var
	path string
}

// startObligations is everything one start body owes its cleanup.
type startObligations struct {
	rawGo      int               // go statements owing one completion signal each
	owners     []map[string]bool // per relation owner: normalized group paths launched through it
	foreign    []string          // displays of groups rooted outside every owner
	unresolved bool              // a WaitGroup launch whose provenance could not be normalized
}

// cleanupEvidence is what one cleanup method visibly proves.
type cleanupEvidence struct {
	receives int             // channel receives, discharging raw go obligations by count
	waits    map[string]bool // normalized receiver-relative WaitGroup.Wait paths
}

func checkGoroutineOwnership(pass *analysis.Pass, lifecycle map[*types.Func]*lifecycleStart) {
	for _, relation := range lifecycle {
		obligations := collectStartObligations(pass, relation)
		if obligations.empty() {
			continue
		}
		if !relationHasLocalCleanup(relation) {
			continue
		}
		reportUnownedLaunches(pass, relation, obligations)
		for i, owner := range relation.owners {
			checkOwnerEvidence(pass, relation, owner, obligations, i)
		}
	}
}

func (o startObligations) empty() bool {
	if o.rawGo > 0 || o.unresolved || len(o.foreign) > 0 {
		return false
	}
	for _, paths := range o.owners {
		if len(paths) > 0 {
			return false
		}
	}
	return true
}

func relationHasLocalCleanup(relation *lifecycleStart) bool {
	for _, owner := range relation.owners {
		if hasLocalCleanup(owner.cleanups) {
			return true
		}
	}
	return false
}

func hasLocalCleanup(cleanups []cleanupMethod) bool {
	for _, cleanup := range cleanups {
		if cleanup.info != nil && cleanup.info.decl.Body != nil {
			return true
		}
	}
	return false
}

// reportUnownedLaunches diagnoses launches through groups no lifecycle owner
// root reaches: cleanup evidence is keyed to owners, so nothing can ever
// prove these joined.
func reportUnownedLaunches(pass *analysis.Pass, relation *lifecycleStart, obligations startObligations) {
	pos := relation.start.decl.Name.Pos()
	for _, display := range obligations.foreign {
		pass.Reportf(pos,
			"[SLC103] %s launches lifecycle goroutine(s) on WaitGroup %s outside its lifecycle owner; owner cleanup cannot prove the join",
			relation.start.fn.Name(), display)
	}
	if obligations.unresolved {
		pass.Reportf(pos,
			"[SLC103] %s launches lifecycle goroutine(s) on a WaitGroup whose provenance cannot be resolved to a lifecycle owner; owner cleanup cannot prove the join",
			relation.start.fn.Name())
	}
}

// checkOwnerEvidence requires one cleanup method of the owner to discharge
// every obligation: a Wait on each group path launched through this owner
// and enough channel receives for every raw go. Evidence is never combined
// across cleanup methods; only one of them runs.
func checkOwnerEvidence(pass *analysis.Pass, relation *lifecycleStart, owner lifecycleOwner, obligations startObligations, index int) {
	required := sortedPaths(obligations.owners[index])
	if len(required) == 0 && obligations.rawGo == 0 {
		return
	}
	if !hasLocalCleanup(owner.cleanups) {
		return
	}
	name, best := bestCleanupEvidence(pass, owner.cleanups, required, obligations.rawGo)
	pos := relation.start.decl.Name.Pos()
	if obligations.rawGo > 0 && best.receives < obligations.rawGo {
		pass.Reportf(pos,
			"[SLC103] %s launches %d lifecycle goroutine(s) but %s visibly waits for only %d completion signal(s); cleanup may return while owned goroutines still run",
			relation.start.fn.Name(), obligations.rawGo, name, best.receives)
	}
	for _, path := range required {
		if best.waits[path] {
			continue
		}
		pass.Reportf(pos,
			"[SLC103] %s launches lifecycle goroutine(s) on WaitGroup %s but %s never waits on that group; cleanup may return while owned goroutines still run",
			relation.start.fn.Name(), ownerGroupDisplay(owner, relation, path), name)
	}
}

// bestCleanupEvidence evaluates each cleanup method independently and
// returns the one covering the most required group paths, breaking ties by
// receive count, so the diagnostic names the closest candidate.
func bestCleanupEvidence(pass *analysis.Pass, cleanups []cleanupMethod, required []string, rawGo int) (string, cleanupEvidence) {
	bestName := "cleanup"
	var best cleanupEvidence
	bestScore := -1
	for _, cleanup := range cleanups {
		if cleanup.info == nil || cleanup.info.decl.Body == nil {
			continue
		}
		evidence := collectCleanupEvidence(pass, cleanup.info)
		score := coveredPaths(evidence, required)
		if score == len(required) && evidence.receives >= rawGo {
			return cleanup.fn.Name(), evidence
		}
		if score > bestScore || (score == bestScore && evidence.receives > best.receives) {
			bestName = cleanup.fn.Name()
			best = evidence
			bestScore = score
		}
	}
	return bestName, best
}

// coveredPaths counts how many required group paths one cleanup waits on.
func coveredPaths(evidence cleanupEvidence, required []string) int {
	score := 0
	for _, path := range required {
		if evidence.waits[path] {
			score++
		}
	}
	return score
}

func sortedPaths(paths map[string]bool) []string {
	out := make([]string, 0, len(paths))
	for path := range paths {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

// ownerGroupDisplay renders an owner-relative group path for a diagnostic,
// e.g. "dockerRun.wg" for path ".wg" on the dockerRun owner.
func ownerGroupDisplay(owner lifecycleOwner, relation *lifecycleStart, path string) string {
	name := "owner"
	if named := ownerNamedType(owner, relation); named != nil {
		name = named.Obj().Name()
	}
	return name + path
}

// ownerNamedType resolves the owner's named type from its result position or
// the start receiver.
func ownerNamedType(owner lifecycleOwner, relation *lifecycleStart) *types.Named {
	sig, _ := relation.start.fn.Type().(*types.Signature)
	if sig == nil {
		return nil
	}
	if owner.result >= 0 && owner.result < sig.Results().Len() {
		return namedType(sig.Results().At(owner.result).Type())
	}
	if sig.Recv() != nil {
		return namedType(sig.Recv().Type())
	}
	return nil
}

// collectStartObligations records every lifecycle launch in the start body,
// attributed by normalized group provenance. Function literals stay inert
// except the launched literal of a go statement, which is inspected only
// for the conventional deferred Done.
func collectStartObligations(pass *analysis.Pass, relation *lifecycleStart) startObligations {
	body := relation.start.decl.Body
	aliases := collectAliases(pass, body)
	ownerRoots := collectOwnerRoots(pass, relation, aliases)
	obligations := startObligations{owners: make([]map[string]bool, len(relation.owners))}
	for i := range obligations.owners {
		obligations.owners[i] = make(map[string]bool)
	}
	adds := collectAddPaths(pass, body, aliases)
	foreign := make(map[string]bool)
	ast.Inspect(body, func(node ast.Node) bool {
		switch n := node.(type) {
		case nil, *ast.FuncLit:
			return false
		case *ast.GoStmt:
			classifyGoLaunch(pass, n, aliases, adds, ownerRoots, &obligations, foreign)
		case *ast.CallExpr:
			if isSyncMethodCall(pass, n, "WaitGroup", "Go") {
				recordGroupLaunch(pass, n, aliases, ownerRoots, &obligations, foreign)
			}
		}
		return true
	})
	obligations.foreign = sortedPaths(foreign)
	return obligations
}

// resolveLaunchGroup normalizes a launch call's receiver expression.
func resolveLaunchGroup(pass *analysis.Pass, call *ast.CallExpr, aliases map[*types.Var]aliasTarget) (groupKey, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return groupKey{}, false
	}
	root, path, ok := resolveGroupPath(pass, sel.X, aliases)
	if !ok {
		return groupKey{}, false
	}
	return groupKey{root: root, path: path}, true
}

// classifyGoLaunch resolves one go statement: a launched literal carrying a
// deferred Done on a group with a matching Add in the start body is that
// group's obligation; everything else, ambiguity included, owes a raw
// completion signal.
func classifyGoLaunch(pass *analysis.Pass, stmt *ast.GoStmt, aliases map[*types.Var]aliasTarget, adds map[groupKey]bool, ownerRoots map[*types.Var]int, obligations *startObligations, foreign map[string]bool) {
	lit, ok := stmt.Call.Fun.(*ast.FuncLit)
	if !ok {
		obligations.rawGo++
		return
	}
	key, ok := deferredDoneGroup(pass, lit, aliases)
	if !ok || !adds[key] {
		obligations.rawGo++
		return
	}
	attributeGroup(key, ownerRoots, obligations, foreign)
}

// deferredDoneGroup finds the single normalized group a launched literal
// defers Done on; two distinct groups or an unresolvable receiver are
// ambiguous and resolve to no group.
func deferredDoneGroup(pass *analysis.Pass, lit *ast.FuncLit, aliases map[*types.Var]aliasTarget) (groupKey, bool) {
	var found groupKey
	count := 0
	ast.Inspect(lit.Body, func(node ast.Node) bool {
		switch n := node.(type) {
		case nil, *ast.FuncLit:
			return false
		case *ast.DeferStmt:
			if !isSyncMethodCall(pass, n.Call, "WaitGroup", "Done") {
				return true
			}
			sel, ok := n.Call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			root, path, ok := resolveGroupPath(pass, sel.X, aliases)
			if !ok {
				count = 2 // unresolvable Done: fail closed
				return true
			}
			key := groupKey{root: root, path: path}
			if count == 0 || key == found {
				found = key
				count = 1
				return true
			}
			count = 2
		}
		return true
	})
	return found, count == 1
}

// recordGroupLaunch attributes one WaitGroup.Go call by its normalized
// receiver provenance.
func recordGroupLaunch(pass *analysis.Pass, call *ast.CallExpr, aliases map[*types.Var]aliasTarget, ownerRoots map[*types.Var]int, obligations *startObligations, foreign map[string]bool) {
	key, ok := resolveLaunchGroup(pass, call, aliases)
	if !ok {
		obligations.unresolved = true
		return
	}
	attributeGroup(key, ownerRoots, obligations, foreign)
}

// attributeGroup files one group obligation under its owner, or under
// foreign/unresolved when no owner root reaches it. A root ambiguous
// between two owners fails closed as unresolved.
func attributeGroup(key groupKey, ownerRoots map[*types.Var]int, obligations *startObligations, foreign map[string]bool) {
	if index, ok := ownerRoots[key.root]; ok {
		if index < 0 {
			obligations.unresolved = true
			return
		}
		obligations.owners[index][key.path] = true
		return
	}
	if named := namedType(key.root.Type()); named != nil {
		foreign[named.Obj().Name()+key.path] = true
		return
	}
	obligations.unresolved = true
}

// collectAddPaths records the normalized groups the start body calls Add on
// outside function literals, so a deferred Done can only pair with an Add
// on the same group.
func collectAddPaths(pass *analysis.Pass, body *ast.BlockStmt, aliases map[*types.Var]aliasTarget) map[groupKey]bool {
	adds := make(map[groupKey]bool)
	ast.Inspect(body, func(node ast.Node) bool {
		switch n := node.(type) {
		case nil, *ast.FuncLit:
			return false
		case *ast.CallExpr:
			if !isSyncMethodCall(pass, n, "WaitGroup", "Add") {
				return true
			}
			sel, ok := n.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if root, path, ok := resolveGroupPath(pass, sel.X, aliases); ok {
				adds[groupKey{root: root, path: path}] = true
			}
		}
		return true
	})
	return adds
}

// collectOwnerRoots maps start-body variables to the owner they root: the
// receiver for a receiver-owned relation, and the variable provably
// returned at an owner result position. Ambiguity fails closed with the
// sentinel index -1: a variable feeding two different owners, or a result
// position fed by two distinct variables — a launch through a root that is
// only conditionally the returned owner must never be discharged by
// evidence that may run on a different object.
func collectOwnerRoots(pass *analysis.Pass, relation *lifecycleStart, aliases map[*types.Var]aliasTarget) map[*types.Var]int {
	roots := make(map[*types.Var]int)
	assign := func(v *types.Var, index int) {
		if existing, ok := roots[v]; ok && existing != index {
			roots[v] = -1
			return
		}
		roots[v] = index
	}
	for i, owner := range relation.owners {
		if owner.result < 0 {
			if recv := receiverVar(pass, relation.start.decl); recv != nil {
				assign(recv, i)
			}
			continue
		}
		vars := returnedVars(pass, relation.start, owner.result, aliases)
		for _, v := range vars {
			if len(vars) > 1 {
				roots[v] = -1
				continue
			}
			assign(v, i)
		}
	}
	return roots
}

// receiverVar resolves a method declaration's named receiver variable.
func receiverVar(pass *analysis.Pass, decl *ast.FuncDecl) *types.Var {
	if decl.Recv == nil || len(decl.Recv.List) == 0 || len(decl.Recv.List[0].Names) == 0 {
		return nil
	}
	v, _ := pass.TypesInfo.Defs[decl.Recv.List[0].Names[0]].(*types.Var)
	return v
}

// returnedVars collects the distinct variables the start body returns at
// one result position, through simple aliases; non-variable results
// contribute nothing.
func returnedVars(pass *analysis.Pass, start *functionInfo, result int, aliases map[*types.Var]aliasTarget) []*types.Var {
	sig, _ := start.fn.Type().(*types.Signature)
	if sig == nil {
		return nil
	}
	seen := make(map[*types.Var]bool)
	var out []*types.Var
	ast.Inspect(start.decl.Body, func(node ast.Node) bool {
		switch n := node.(type) {
		case nil, *ast.FuncLit:
			return false
		case *ast.ReturnStmt:
			if len(n.Results) != sig.Results().Len() || result >= len(n.Results) {
				return true
			}
			if root, path, ok := resolveGroupPath(pass, n.Results[result], aliases); ok && path == "" && !seen[root] {
				seen[root] = true
				out = append(out, root)
			}
		}
		return true
	})
	return out
}

// collectCleanupEvidence gathers one cleanup body's receive count and the
// receiver-relative group paths it waits on. Function literals stay inert
// except typed sync.Once.Do callbacks, which run within the cleanup call.
func collectCleanupEvidence(pass *analysis.Pass, info *functionInfo) cleanupEvidence {
	evidence := cleanupEvidence{waits: make(map[string]bool)}
	recv := receiverVar(pass, info.decl)
	aliases := collectAliases(pass, info.decl.Body)
	var inspect func(ast.Node)
	inspect = func(root ast.Node) {
		ast.Inspect(root, func(node ast.Node) bool {
			switch n := node.(type) {
			case nil, *ast.FuncLit:
				return false
			case *ast.UnaryExpr:
				if n.Op == token.ARROW {
					evidence.receives++
				}
			case *ast.CallExpr:
				scanCleanupCall(pass, n, recv, aliases, &evidence, inspect)
			}
			return true
		})
	}
	inspect(info.decl.Body)
	return evidence
}

// scanCleanupCall records one call's wait evidence and follows sync.Once.Do
// callbacks.
func scanCleanupCall(pass *analysis.Pass, call *ast.CallExpr, recv *types.Var, aliases map[*types.Var]aliasTarget, evidence *cleanupEvidence, inspect func(ast.Node)) {
	if isSyncMethodCall(pass, call, "WaitGroup", "Wait") {
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && recv != nil {
			if root, path, ok := resolveGroupPath(pass, sel.X, aliases); ok && root == recv {
				evidence.waits[path] = true
			}
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

// resolveGroupPath normalizes expr to a root variable plus the complete
// field-selection path below it, following parens, address-of, dereference,
// and single-assignment aliases. Anything else is unresolvable.
func resolveGroupPath(pass *analysis.Pass, expr ast.Expr, aliases map[*types.Var]aliasTarget) (*types.Var, string, bool) {
	switch e := ast.Unparen(expr).(type) {
	case *ast.Ident:
		v, _ := pass.TypesInfo.Uses[e].(*types.Var)
		if v == nil {
			return nil, "", false
		}
		if target, ok := aliases[v]; ok {
			return target.root, target.path, true
		}
		return v, "", true
	case *ast.SelectorExpr:
		return resolveSelectorPath(pass, e, aliases)
	case *ast.UnaryExpr:
		if e.Op == token.AND {
			return resolveGroupPath(pass, e.X, aliases)
		}
	case *ast.StarExpr:
		return resolveGroupPath(pass, e.X, aliases)
	}
	return nil, "", false
}

// resolveSelectorPath normalizes one field selection step on top of its
// resolved base.
func resolveSelectorPath(pass *analysis.Pass, e *ast.SelectorExpr, aliases map[*types.Var]aliasTarget) (*types.Var, string, bool) {
	sel := pass.TypesInfo.Selections[e]
	if sel == nil || sel.Kind() != types.FieldVal {
		return nil, "", false
	}
	root, base, ok := resolveGroupPath(pass, e.X, aliases)
	if !ok {
		return nil, "", false
	}
	path, ok := selectionFieldPath(sel)
	if !ok {
		return nil, "", false
	}
	return root, base + path, true
}

// selectionFieldPath renders a field selection's complete index path as
// ".f" segments, so embedded promotion still yields the full path. An
// index that cannot be mapped to a struct field is unresolvable rather
// than a collidable placeholder.
func selectionFieldPath(sel *types.Selection) (string, bool) {
	var b strings.Builder
	current := sel.Recv()
	for _, index := range sel.Index() {
		st := underlyingStruct(current)
		if st == nil || index >= st.NumFields() {
			return "", false
		}
		field := st.Field(index)
		b.WriteString(".")
		b.WriteString(field.Name())
		current = field.Type()
	}
	return b.String(), true
}

// underlyingStruct dereferences pointers and named types down to a struct.
func underlyingStruct(t types.Type) *types.Struct {
	for {
		t = types.Unalias(t)
		if pointer, ok := t.(*types.Pointer); ok {
			t = pointer.Elem()
			continue
		}
		break
	}
	st, _ := t.Underlying().(*types.Struct)
	return st
}

// collectAliases maps single-assignment local variables to what they alias:
// x := y, x := &y.f, or x := y.f chains rooted at another variable. A
// variable written more than once, or whose address is taken, is ambiguous
// and never counts as an alias.
func collectAliases(pass *analysis.Pass, body *ast.BlockStmt) map[*types.Var]aliasTarget {
	writes := countVarWrites(pass, body)
	candidates := aliasCandidates(pass, body, writes)
	aliases := make(map[*types.Var]aliasTarget)
	for changed := true; changed; {
		changed = false
		for v, rhs := range candidates {
			if _, done := aliases[v]; done {
				continue
			}
			root, path, ok := resolveGroupPath(pass, rhs, aliases)
			if !ok || root == v {
				continue
			}
			aliases[v] = aliasTarget{root: root, path: path}
			changed = true
		}
	}
	return aliases
}

// aliasCandidates returns the := definitions eligible to alias, keyed by
// the defined variable.
func aliasCandidates(pass *analysis.Pass, body *ast.BlockStmt, writes map[*types.Var]int) map[*types.Var]ast.Expr {
	candidates := make(map[*types.Var]ast.Expr)
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
			v, _ := pass.TypesInfo.Defs[id].(*types.Var)
			if v == nil || writes[v] != 1 {
				continue
			}
			candidates[v] = assign.Rhs[i]
		}
		return true
	})
	return candidates
}

// countVarWrites counts assignments to each local variable, address-of uses
// included: an address escape makes later mutation invisible to this model.
func countVarWrites(pass *analysis.Pass, body *ast.BlockStmt) map[*types.Var]int {
	writes := make(map[*types.Var]int)
	record := func(id *ast.Ident) {
		if v, _ := pass.TypesInfo.Defs[id].(*types.Var); v != nil {
			writes[v]++
			return
		}
		if v, _ := pass.TypesInfo.Uses[id].(*types.Var); v != nil {
			writes[v]++
		}
	}
	ast.Inspect(body, func(node ast.Node) bool {
		recordNodeWrites(node, record)
		return true
	})
	return writes
}

// recordNodeWrites feeds every identifier one node writes, or takes the
// address of, into record.
func recordNodeWrites(node ast.Node, record func(*ast.Ident)) {
	recordIdent := func(expr ast.Expr) {
		if id, ok := expr.(*ast.Ident); ok {
			record(id)
		}
	}
	switch n := node.(type) {
	case *ast.AssignStmt:
		for _, lhs := range n.Lhs {
			recordIdent(lhs)
		}
	case *ast.IncDecStmt:
		recordIdent(n.X)
	case *ast.RangeStmt:
		recordIdent(n.Key)
		recordIdent(n.Value)
	case *ast.UnaryExpr:
		if n.Op == token.AND {
			recordIdent(ast.Unparen(n.X))
		}
	}
}
