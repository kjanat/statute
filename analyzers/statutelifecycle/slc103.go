package statutelifecycle

// SLC103 models goroutine ownership as obligations against evidence.
// WaitGroup launches carry exact provenance: each owes a Wait on the very
// same group, normalized to lifecycle owner root plus the complete
// field-selection path, resolved only through variables the body never
// reassigns and aliases that preserve storage identity. A Wait on one
// group can never discharge work launched through another group, another
// owner, or a raw go, and a launch whose group cannot be attributed to a
// lifecycle owner is undischargeable and diagnosed: unknown provenance
// fails closed. Raw go statements are deliberately count-based: each owes
// one completion signal, discharged by visible channel receives in the
// cleanup by count — channel identity is out of SLC103's scope (issue #62
// non-goals), so this is conservative join evidence, not proof that a
// particular receive joins a particular goroutine.

import (
	"go/ast"
	"go/token"
	"go/types"
	"sort"
	"strconv"
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
	resolver := newPathResolver(pass, body)
	ownerRoots := collectOwnerRoots(pass, relation, resolver)
	obligations := startObligations{owners: make([]map[string]bool, len(relation.owners))}
	for i := range obligations.owners {
		obligations.owners[i] = make(map[string]bool)
	}
	capacity := collectAddCapacity(pass, body, resolver)
	foreign := make(map[string]bool)
	ast.Inspect(body, func(node ast.Node) bool {
		switch n := node.(type) {
		case nil, *ast.FuncLit:
			return false
		case *ast.GoStmt:
			classifyGoLaunch(pass, n, resolver, capacity, ownerRoots, &obligations, foreign)
		case *ast.CallExpr:
			if isSyncMethodCall(pass, n, "WaitGroup", "Go") {
				recordGroupLaunch(n, resolver, ownerRoots, &obligations, foreign)
			}
		}
		return true
	})
	obligations.foreign = sortedPaths(foreign)
	return obligations
}

// resolveLaunchGroup normalizes a launch call's receiver expression.
func resolveLaunchGroup(call *ast.CallExpr, resolver *pathResolver) (groupKey, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return groupKey{}, false
	}
	root, path, ok := resolver.resolve(sel.X)
	if !ok {
		return groupKey{}, false
	}
	return groupKey{root: root, path: path}, true
}

// classifyGoLaunch resolves one go statement: a launched literal carrying a
// deferred Done on a group with unspent Add(1)-style registration capacity
// earlier in the start body is that group's obligation; everything else,
// ambiguity included, owes a raw completion signal.
func classifyGoLaunch(pass *analysis.Pass, stmt *ast.GoStmt, resolver *pathResolver, capacity *addCapacity, ownerRoots map[*types.Var]int, obligations *startObligations, foreign map[string]bool) {
	lit, ok := stmt.Call.Fun.(*ast.FuncLit)
	if !ok {
		obligations.rawGo++
		return
	}
	key, ok := deferredDoneGroup(pass, lit, resolver)
	if !ok || !capacity.take(key, stmt.Pos()) {
		obligations.rawGo++
		return
	}
	attributeGroup(key, ownerRoots, obligations, foreign)
}

// deferredDoneGroup finds the single normalized group a launched literal
// defers Done on; two distinct groups or an unresolvable receiver are
// ambiguous and resolve to no group.
func deferredDoneGroup(pass *analysis.Pass, lit *ast.FuncLit, resolver *pathResolver) (groupKey, bool) {
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
			root, path, ok := resolver.resolve(sel.X)
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
func recordGroupLaunch(call *ast.CallExpr, resolver *pathResolver, ownerRoots map[*types.Var]int, obligations *startObligations, foreign map[string]bool) {
	key, ok := resolveLaunchGroup(call, resolver)
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
		display := named.Obj().Name() + key.path
		if key.path == "" {
			display = key.root.Name()
		}
		foreign[display] = true
		return
	}
	obligations.unresolved = true
}

// addEvent is one WaitGroup.Add call carrying a constant positive
// registration count.
type addEvent struct {
	pos token.Pos
	n   int
}

// addCapacity tracks how much Add-registered capacity each normalized group
// has left, in source order, so a deferred Done can only claim registration
// that provably precedes its launch and each unit is spent once. One Add(1)
// cannot vouch for two goroutines: whichever calls Done first would release
// Wait while the other still runs.
type addCapacity struct {
	events map[groupKey][]addEvent
	used   map[groupKey]int
}

// take consumes one unit of registration capacity recorded before pos.
func (c *addCapacity) take(key groupKey, pos token.Pos) bool {
	available := 0
	for _, event := range c.events[key] {
		if event.pos < pos {
			available += event.n
		}
	}
	if available-c.used[key] < 1 {
		return false
	}
	c.used[key]++
	return true
}

// collectAddCapacity records the start body's WaitGroup.Add calls per
// normalized group, outside function literals. Only a constant positive
// count registers capacity: capacity the model cannot see is capacity the
// deferred Done pattern cannot spend.
func collectAddCapacity(pass *analysis.Pass, body *ast.BlockStmt, resolver *pathResolver) *addCapacity {
	capacity := &addCapacity{events: make(map[groupKey][]addEvent), used: make(map[groupKey]int)}
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
			count, ok := constantAddCount(n)
			if !ok {
				return true
			}
			if root, path, ok := resolver.resolve(sel.X); ok {
				key := groupKey{root: root, path: path}
				capacity.events[key] = append(capacity.events[key], addEvent{pos: n.Pos(), n: count})
			}
		}
		return true
	})
	return capacity
}

// constantAddCount extracts an Add call's registration count when it is a
// positive integer literal.
func constantAddCount(call *ast.CallExpr) (int, bool) {
	if len(call.Args) != 1 {
		return 0, false
	}
	lit, ok := ast.Unparen(call.Args[0]).(*ast.BasicLit)
	if !ok || lit.Kind != token.INT {
		return 0, false
	}
	n, err := strconv.Atoi(lit.Value)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// collectOwnerRoots maps start-body variables to the owner they root: the
// receiver for a receiver-owned relation, and the variable provably
// returned at an owner result position. Ambiguity fails closed with the
// sentinel index -1: a variable feeding two different owners, or a result
// position fed by two distinct variables — a launch through a root that is
// only conditionally the returned owner must never be discharged by
// evidence that may run on a different object.
func collectOwnerRoots(pass *analysis.Pass, relation *lifecycleStart, resolver *pathResolver) map[*types.Var]int {
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
		vars := returnedVars(relation.start, owner.result, resolver)
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
// one result position, through simple aliases; non-variable results and
// variables the body reassigns contribute nothing.
func returnedVars(start *functionInfo, result int, resolver *pathResolver) []*types.Var {
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
			if root, path, ok := resolver.resolve(n.Results[result]); ok && path == "" && !seen[root] {
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
	resolver := newPathResolver(pass, info.decl.Body)
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
				scanCleanupCall(pass, n, recv, resolver, &evidence, inspect)
			}
			return true
		})
	}
	inspect(info.decl.Body)
	return evidence
}

// scanCleanupCall records one call's wait evidence and follows sync.Once.Do
// callbacks.
func scanCleanupCall(pass *analysis.Pass, call *ast.CallExpr, recv *types.Var, resolver *pathResolver, evidence *cleanupEvidence, inspect func(ast.Node)) {
	if isSyncMethodCall(pass, call, "WaitGroup", "Wait") {
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && recv != nil {
			if root, path, ok := resolver.resolve(sel.X); ok && root == recv {
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

// pathResolver normalizes expressions within one function body. A variable
// the body reassigns after its definition, or takes the address of, is
// never resolvable: it may denote different objects at different points,
// and guessing which one would let evidence discharge work that lives on
// another object. Aliases resolve only through pointer-typed definitions,
// because only a pointer preserves the identity of the storage it names.
type pathResolver struct {
	pass    *analysis.Pass
	aliases map[*types.Var]aliasTarget
	mutated map[*types.Var]bool
}

func newPathResolver(pass *analysis.Pass, body *ast.BlockStmt) *pathResolver {
	r := &pathResolver{
		pass:    pass,
		aliases: make(map[*types.Var]aliasTarget),
		mutated: collectMutatedVars(pass, body),
	}
	r.collectAliases(body)
	return r
}

// resolve normalizes expr to a root variable plus the complete
// field-selection path below it, following parens, address-of, dereference,
// and single-assignment pointer aliases. Anything else is unresolvable.
func (r *pathResolver) resolve(expr ast.Expr) (*types.Var, string, bool) {
	switch e := ast.Unparen(expr).(type) {
	case *ast.Ident:
		v, _ := r.pass.TypesInfo.Uses[e].(*types.Var)
		if v == nil || r.mutated[v] {
			return nil, "", false
		}
		if target, ok := r.aliases[v]; ok {
			return target.root, target.path, true
		}
		return v, "", true
	case *ast.SelectorExpr:
		return r.resolveSelector(e)
	case *ast.UnaryExpr:
		if e.Op == token.AND {
			return r.resolve(e.X)
		}
	case *ast.StarExpr:
		return r.resolve(e.X)
	}
	return nil, "", false
}

// resolveSelector normalizes one field selection step on top of its
// resolved base.
func (r *pathResolver) resolveSelector(e *ast.SelectorExpr) (*types.Var, string, bool) {
	sel := r.pass.TypesInfo.Selections[e]
	if sel == nil || sel.Kind() != types.FieldVal {
		return nil, "", false
	}
	root, base, ok := r.resolve(e.X)
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

// collectAliases resolves the eligible alias definitions transitively:
// run := r, wg := &r.wg, sub := r.a chains rooted at another stable
// variable.
func (r *pathResolver) collectAliases(body *ast.BlockStmt) {
	candidates := r.aliasCandidates(body)
	for changed := true; changed; {
		changed = false
		for v, rhs := range candidates {
			if _, done := r.aliases[v]; done {
				continue
			}
			root, path, ok := r.resolve(rhs)
			if !ok || root == v {
				continue
			}
			r.aliases[v] = aliasTarget{root: root, path: path}
			changed = true
		}
	}
}

// aliasCandidates returns the := definitions eligible to alias, keyed by
// the defined variable. Only a pointer-typed right-hand side preserves the
// identity of the storage it names: x := y.f or x := *p on a struct copies
// the value, and a Wait through the copy proves nothing about the original.
func (r *pathResolver) aliasCandidates(body *ast.BlockStmt) map[*types.Var]ast.Expr {
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
			v, _ := r.pass.TypesInfo.Defs[id].(*types.Var)
			if v == nil || r.mutated[v] || !isPointerType(r.pass.TypesInfo.TypeOf(assign.Rhs[i])) {
				continue
			}
			candidates[v] = assign.Rhs[i]
		}
		return true
	})
	return candidates
}

// isPointerType reports whether t is a pointer after alias unwrapping.
func isPointerType(t types.Type) bool {
	if t == nil {
		return false
	}
	_, ok := types.Unalias(t).(*types.Pointer)
	return ok
}

// collectMutatedVars records every variable the body assigns to after its
// definition, or takes the address of: definitions land in Defs, so a Uses
// identifier on a write side is always a mutation of an existing variable,
// and an address escape makes later mutation invisible to this model.
func collectMutatedVars(pass *analysis.Pass, body *ast.BlockStmt) map[*types.Var]bool {
	mutated := make(map[*types.Var]bool)
	record := func(id *ast.Ident) {
		if v, _ := pass.TypesInfo.Uses[id].(*types.Var); v != nil {
			mutated[v] = true
		}
	}
	ast.Inspect(body, func(node ast.Node) bool {
		recordNodeWrites(node, record)
		return true
	})
	return mutated
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
