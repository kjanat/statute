package statutelifecycle

// SLC103 models goroutine ownership as obligations against evidence.
// WaitGroup launches carry exact provenance: each owes a Wait on the very
// same group, normalized to lifecycle owner root plus the complete
// field-selection path, resolved only through variables the body never
// reassigns and aliases that preserve storage identity. Provenance is
// storage identity, not a lexical path: a write or address escape anywhere
// along the field path — a pointer alias to it leaving the body as a value
// included — replaces or may replace the storage the path names, so the
// whole path becomes unresolvable. A Wait on one group can never discharge
// work launched through another group, another owner, or a raw go, and a
// launch whose group cannot be attributed to a lifecycle owner is
// undischargeable and diagnosed: unknown provenance fails closed. Add(n)
// registration counts only when its statement dominates the launch in the
// block structure — a goto disables registration for the whole body — and
// any counter operation the model cannot account for, function literals
// included, poisons that group's capacity entirely. Raw go statements
// are deliberately count-based: each owes one completion signal,
// discharged by visible channel receives in the cleanup by count —
// channel identity is out of SLC103's scope (issue #62 non-goals), so
// this is conservative join evidence, not proof that a particular receive
// joins a particular goroutine.

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

// resolveReceiverGroup normalizes a group method call's receiver expression.
func resolveReceiverGroup(call *ast.CallExpr, resolver *pathResolver) (groupKey, bool) {
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
	key, ok := resolveReceiverGroup(call, resolver)
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

// blockRef is one step of a statement's position in the nested block
// structure: the enclosing block and the statement index within it.
type blockRef struct {
	block *ast.BlockStmt
	index int
}

// addEvent is one recognized WaitGroup.Add statement carrying a constant
// registration count and its position in the block structure.
type addEvent struct {
	chain []blockRef
	n     int
}

// addCapacity tracks how much Add-registered capacity each normalized group
// has left. Registration counts only when the Add statement dominates the
// launch in the block structure: an Add inside a conditional branch, a
// defer, or after the go statement is not provably executed before the
// goroutine starts. Block ordering is dominance only while control flow is
// structured, so a goto anywhere in the body — a jump can land between
// registration and launch — poisons all capacity. Each unit is spent once
// — one Add(1) cannot vouch for two goroutines — and any counter operation
// the model cannot account for (a Done in the start body or in any
// function literal outside the recognized launched shape, a non-constant
// or negative Add) poisons that group's capacity: the model no longer
// knows the counter's value, so no launch may claim registration through
// it.
type addCapacity struct {
	body        *ast.BlockStmt
	events      map[groupKey][]addEvent
	used        map[groupKey]int
	poisoned    map[groupKey]bool
	poisonedAll bool
}

// take consumes one unit of registration capacity whose Add dominates the
// launch at pos.
func (c *addCapacity) take(key groupKey, pos token.Pos) bool {
	if c.poisonedAll || c.poisoned[key] {
		return false
	}
	goChain := stmtChain(c.body, pos)
	available := 0
	for _, event := range c.events[key] {
		if dominates(event.chain, goChain) {
			available += event.n
		}
	}
	if available-c.used[key] < 1 {
		return false
	}
	c.used[key]++
	return true
}

// stmtChain locates pos in body's nested block structure, one blockRef per
// enclosing block from the outside in.
func stmtChain(body *ast.BlockStmt, pos token.Pos) []blockRef {
	var chain []blockRef
	block := body
	for block != nil {
		next := (*ast.BlockStmt)(nil)
		for i, stmt := range block.List {
			if stmt.Pos() <= pos && pos < stmt.End() {
				chain = append(chain, blockRef{block: block, index: i})
				next = childBlock(stmt, pos)
				break
			}
		}
		block = next
	}
	return chain
}

// childBlock finds the shallowest nested block statement of stmt containing
// pos, without descending into function literals.
func childBlock(stmt ast.Stmt, pos token.Pos) *ast.BlockStmt {
	var found *ast.BlockStmt
	ast.Inspect(stmt, func(node ast.Node) bool {
		if found != nil {
			return false
		}
		switch n := node.(type) {
		case *ast.FuncLit:
			return false
		case *ast.BlockStmt:
			if n.Pos() <= pos && pos < n.End() {
				found = n
			}
			return false
		}
		return true
	})
	return found
}

// dominates reports whether the Add at addChain provably executes before
// every arrival at goChain: the Add sits in a block the launch's chain
// passes through, at an earlier statement index, so structured control flow
// cannot reach the go statement without passing the Add first.
func dominates(addChain, goChain []blockRef) bool {
	if len(addChain) == 0 || len(addChain) > len(goChain) {
		return false
	}
	last := len(addChain) - 1
	for k := range last {
		if addChain[k] != goChain[k] {
			return false
		}
	}
	return addChain[last].block == goChain[last].block && addChain[last].index < goChain[last].index
}

// collectAddCapacity records the start body's WaitGroup counter operations
// per normalized group. Only a plain Add statement with a constant
// non-negative count, directly in the body, is accountable: a positive
// count registers capacity at its block position, zero registers nothing.
// Every other counter operation — Done in the start body proper, an Add
// buried in a defer or expression, a non-constant or negative count, any
// Add or Done inside a function literal other than the launched literal's
// own deferred Done — leaves the counter in a state the model cannot see
// and poisons the group; an operation whose receiver cannot even be
// normalized, or a goto anywhere in the body, poisons all capacity.
func collectAddCapacity(pass *analysis.Pass, body *ast.BlockStmt, resolver *pathResolver) *addCapacity {
	capacity := &addCapacity{
		body:     body,
		events:   make(map[groupKey][]addEvent),
		used:     make(map[groupKey]int),
		poisoned: make(map[groupKey]bool),
	}
	registered := make(map[*ast.CallExpr]bool)
	launched := make(map[*ast.FuncLit]bool)
	ast.Inspect(body, func(node ast.Node) bool {
		return capacity.visitBodyNode(pass, node, resolver, registered, launched)
	})
	return capacity
}

// visitBodyNode handles one node of the start body's capacity walk.
func (c *addCapacity) visitBodyNode(pass *analysis.Pass, node ast.Node, resolver *pathResolver, registered map[*ast.CallExpr]bool, launched map[*ast.FuncLit]bool) bool {
	switch n := node.(type) {
	case nil:
		return false
	case *ast.BranchStmt:
		if n.Tok == token.GOTO {
			c.poisonedAll = true
		}
	case *ast.GoStmt:
		if lit, ok := n.Call.Fun.(*ast.FuncLit); ok {
			launched[lit] = true
			c.scanLaunchedLiteral(pass, lit, resolver)
		}
	case *ast.FuncLit:
		if !launched[n] {
			c.poisonCounterOps(pass, n.Body, resolver)
		}
		return false
	case *ast.ExprStmt:
		c.recordAddStmt(pass, n, resolver, registered)
	case *ast.CallExpr:
		c.recordUnaccountedOp(pass, n, resolver, registered)
	}
	return true
}

// recordAddStmt registers one plain statement's Add call, when it is one.
func (c *addCapacity) recordAddStmt(pass *analysis.Pass, stmt *ast.ExprStmt, resolver *pathResolver, registered map[*ast.CallExpr]bool) {
	call, ok := ast.Unparen(stmt.X).(*ast.CallExpr)
	if !ok || !isSyncMethodCall(pass, call, "WaitGroup", "Add") {
		return
	}
	registered[call] = true
	c.recordAdd(call, resolver)
}

// scanLaunchedLiteral inspects a go statement's own function literal: its
// deferred Done calls are the recognized discharge shape and stay exempt,
// but any other counter operation inside it — a plain Done, an Add, or
// anything in a nested literal — mutates the counter at a time the
// registration model cannot order and poisons the group.
func (c *addCapacity) scanLaunchedLiteral(pass *analysis.Pass, lit *ast.FuncLit, resolver *pathResolver) {
	exempt := make(map[*ast.CallExpr]bool)
	ast.Inspect(lit.Body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.FuncLit:
			c.poisonCounterOps(pass, n.Body, resolver)
			return false
		case *ast.DeferStmt:
			if isSyncMethodCall(pass, n.Call, "WaitGroup", "Done") {
				exempt[n.Call] = true
			}
		case *ast.CallExpr:
			if !exempt[n] && (isSyncMethodCall(pass, n, "WaitGroup", "Done") || isSyncMethodCall(pass, n, "WaitGroup", "Add")) {
				c.poisonCall(n, resolver)
			}
		}
		return true
	})
}

// poisonCounterOps poisons the group of every WaitGroup Add or Done found
// under node, nested literals included: counter operations inside an
// ordinary function literal run at times the registration model cannot
// account for.
func (c *addCapacity) poisonCounterOps(pass *analysis.Pass, node ast.Node, resolver *pathResolver) {
	ast.Inspect(node, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if isSyncMethodCall(pass, call, "WaitGroup", "Done") || isSyncMethodCall(pass, call, "WaitGroup", "Add") {
				c.poisonCall(call, resolver)
			}
		}
		return true
	})
}

// poisonCall poisons one counter operation's group, or all capacity when
// its receiver cannot be normalized.
func (c *addCapacity) poisonCall(call *ast.CallExpr, resolver *pathResolver) {
	if key, ok := resolveReceiverGroup(call, resolver); ok {
		c.poisoned[key] = true
		return
	}
	c.poisonedAll = true
}

// recordAdd accounts one plain Add statement: constant positive counts
// register capacity, zero is inert, anything else poisons.
func (c *addCapacity) recordAdd(call *ast.CallExpr, resolver *pathResolver) {
	key, ok := resolveReceiverGroup(call, resolver)
	if !ok {
		c.poisonedAll = true
		return
	}
	count, ok := constantAddCount(call)
	switch {
	case !ok:
		c.poisoned[key] = true
	case count > 0:
		c.events[key] = append(c.events[key], addEvent{chain: stmtChain(c.body, call.Pos()), n: count})
	}
}

// recordUnaccountedOp poisons the group of a counter operation the
// registration model cannot account for: any Done in the start body
// proper, or an Add outside a plain statement position.
func (c *addCapacity) recordUnaccountedOp(pass *analysis.Pass, call *ast.CallExpr, resolver *pathResolver, registered map[*ast.CallExpr]bool) {
	isDone := isSyncMethodCall(pass, call, "WaitGroup", "Done")
	isAdd := !isDone && isSyncMethodCall(pass, call, "WaitGroup", "Add") && !registered[call]
	if !isDone && !isAdd {
		return
	}
	c.poisonCall(call, resolver)
}

// constantAddCount extracts an Add call's registration count when it is a
// non-negative integer literal.
func constantAddCount(call *ast.CallExpr) (int, bool) {
	if len(call.Args) != 1 {
		return 0, false
	}
	lit, ok := ast.Unparen(call.Args[0]).(*ast.BasicLit)
	if !ok || lit.Kind != token.INT {
		return 0, false
	}
	n, err := strconv.Atoi(lit.Value)
	if err != nil || n < 0 {
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
// another object. The same holds one selector deeper: a write or address
// escape anywhere along a field path may replace the storage the path
// names, so every path below the written prefix is unresolvable too.
// Aliases resolve only through pointer-typed definitions, because only a
// pointer preserves the identity of the storage it names.
type pathResolver struct {
	pass        *analysis.Pass
	aliases     map[*types.Var]aliasTarget
	aliasRHS    map[ast.Expr]bool
	addrAliases map[*types.Var]bool
	written     map[*types.Var][]string
	mutated     map[*types.Var]bool
}

func newPathResolver(pass *analysis.Pass, body *ast.BlockStmt) *pathResolver {
	r := &pathResolver{
		pass:        pass,
		aliases:     make(map[*types.Var]aliasTarget),
		aliasRHS:    make(map[ast.Expr]bool),
		addrAliases: make(map[*types.Var]bool),
		written:     make(map[*types.Var][]string),
		mutated:     collectMutatedVars(pass, body),
	}
	r.collectAliases(body)
	r.collectWrittenPaths(body)
	r.collectAliasEscapes(body)
	return r
}

// resolve normalizes expr to a root variable plus the complete
// field-selection path below it, then refuses the result when the body
// writes to, or lets escape, any prefix of that path: the storage the
// path named at one point may not be the storage it names at another.
func (r *pathResolver) resolve(expr ast.Expr) (*types.Var, string, bool) {
	root, path, ok := r.resolveExpr(expr)
	if !ok || r.pathInvalidated(root, path) {
		return nil, "", false
	}
	return root, path, true
}

// pathInvalidated reports whether a written or escaped path covers path:
// equal to it, or a segment-aligned prefix of it.
func (r *pathResolver) pathInvalidated(root *types.Var, path string) bool {
	for _, written := range r.written[root] {
		if written == path || strings.HasPrefix(path, written+".") {
			return true
		}
	}
	return false
}

// resolveExpr normalizes expr to a root variable plus the complete
// field-selection path below it, following parens, address-of, dereference,
// and single-assignment pointer aliases. Anything else is unresolvable.
func (r *pathResolver) resolveExpr(expr ast.Expr) (*types.Var, string, bool) {
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
			return r.resolveExpr(e.X)
		}
	case *ast.StarExpr:
		return r.resolveExpr(e.X)
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
	root, base, ok := r.resolveExpr(e.X)
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
			r.aliasRHS[rhs] = true
			if r.aliasTakesAddress(rhs) {
				r.addrAliases[v] = true
			}
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

// collectWrittenPaths records every field path the body writes through, or
// lets escape by address, keyed by resolved root. Bare identifiers are the
// mutated-variable model's job; an address-of expression is exempt only
// when it is the right-hand side of an accepted alias definition, because
// writes through that alias are themselves resolved and recorded here.
// Function literals are included: a write from a launched goroutine
// replaces storage just as effectively.
func (r *pathResolver) collectWrittenPaths(body *ast.BlockStmt) {
	ast.Inspect(body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.AssignStmt:
			for _, lhs := range n.Lhs {
				r.recordWrittenPath(lhs)
			}
		case *ast.IncDecStmt:
			r.recordWrittenPath(n.X)
		case *ast.RangeStmt:
			r.recordWrittenPath(n.Key)
			r.recordWrittenPath(n.Value)
		case *ast.UnaryExpr:
			if n.Op == token.AND && !r.aliasRHS[n] {
				r.recordWrittenPath(n.X)
			}
		}
		return true
	})
}

// aliasTakesAddress reports whether an accepted alias definition names the
// address of a field cell rather than copying an existing pointer: its
// right-hand side takes an address directly, or is an identifier whose own
// alias already does.
func (r *pathResolver) aliasTakesAddress(rhs ast.Expr) bool {
	switch e := ast.Unparen(rhs).(type) {
	case *ast.UnaryExpr:
		return e.Op == token.AND
	case *ast.Ident:
		v, _ := r.pass.TypesInfo.Uses[e].(*types.Var)
		return v != nil && r.addrAliases[v]
	}
	return false
}

// collectAliasEscapes invalidates the target path of every address-taken
// alias the body uses as a value: passing, storing, comparing, or
// returning the pointer hands replacement of the aliased storage to code
// the local model cannot see. Using the alias as a selector or
// dereference base stays safe — those reads and writes are resolved and
// recorded through the alias itself.
func (r *pathResolver) collectAliasEscapes(body *ast.BlockStmt) {
	if len(r.addrAliases) == 0 {
		return
	}
	safe := safeAliasBaseUses(body)
	ast.Inspect(body, func(node ast.Node) bool {
		id, ok := node.(*ast.Ident)
		if !ok || safe[id] || r.aliasRHS[id] {
			return true
		}
		v, _ := r.pass.TypesInfo.Uses[id].(*types.Var)
		if v == nil || !r.addrAliases[v] {
			return true
		}
		target := r.aliases[v]
		r.written[target.root] = append(r.written[target.root], target.path)
		return true
	})
}

// safeAliasBaseUses collects the identifier occurrences used as selector or
// dereference bases: reads and writes through those go through path
// resolution and are accounted elsewhere.
func safeAliasBaseUses(body *ast.BlockStmt) map[*ast.Ident]bool {
	safe := make(map[*ast.Ident]bool)
	mark := func(expr ast.Expr) {
		if id, ok := ast.Unparen(expr).(*ast.Ident); ok {
			safe[id] = true
		}
	}
	ast.Inspect(body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.SelectorExpr:
			mark(n.X)
		case *ast.StarExpr:
			mark(n.X)
		}
		return true
	})
	return safe
}

// recordWrittenPath resolves one written or escaped expression and records
// its path under the resolved root.
func (r *pathResolver) recordWrittenPath(expr ast.Expr) {
	if expr == nil {
		return
	}
	if _, ok := ast.Unparen(expr).(*ast.Ident); ok {
		return
	}
	if root, path, ok := r.resolveExpr(expr); ok {
		r.written[root] = append(r.written[root], path)
	}
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
