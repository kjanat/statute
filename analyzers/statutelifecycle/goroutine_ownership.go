package statutelifecycle

// SLC103 fails closed when WaitGroup storage identity cannot be proven.

import (
	"go/ast"
	"go/token"
	"go/types"
	"sort"
	"strconv"

	"golang.org/x/tools/go/analysis"
)

// groupKey identifies one normalized WaitGroup: the root variable it is
// selected from and the complete field-selection path below that root.
type groupKey struct {
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
			"["+diagnosticSLC103+"] %s launches lifecycle goroutine(s) on WaitGroup %s outside its lifecycle owner; owner cleanup cannot prove the join",
			relation.start.fn.Name(), display)
	}
	if obligations.unresolved {
		pass.Reportf(pos,
			"["+diagnosticSLC103+"] %s launches lifecycle goroutine(s) on a WaitGroup whose provenance cannot be resolved to a lifecycle owner; owner cleanup cannot prove the join",
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
			"["+diagnosticSLC103+"] %s launches %d lifecycle goroutine(s) but %s visibly waits for only %d completion signal(s); cleanup may return while owned goroutines still run",
			relation.start.fn.Name(), obligations.rawGo, name, best.receives)
	}
	for _, path := range required {
		if best.waits[path] {
			continue
		}
		pass.Reportf(pos,
			"["+diagnosticSLC103+"] %s launches lifecycle goroutine(s) on WaitGroup %s but %s never waits on that group; cleanup may return while owned goroutines still run",
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

// classifyGoLaunch attributes a recognized deferred Done to available,
// dominating Add capacity. Other launches become raw obligations; an
// unaccounted Done poisons the group's remaining capacity.
func classifyGoLaunch(pass *analysis.Pass, stmt *ast.GoStmt, resolver *pathResolver, capacity *addCapacity, ownerRoots map[*types.Var]int, obligations *startObligations, foreign map[string]bool) {
	lit, ok := stmt.Call.Fun.(*ast.FuncLit)
	if !ok {
		obligations.rawGo++
		return
	}
	key, ok := deferredDoneGroup(pass, lit, resolver)
	if !ok {
		obligations.rawGo++
		return
	}
	if !capacity.take(key, stmt.Pos()) {
		capacity.poisoned[key] = true
		obligations.rawGo++
		return
	}
	attributeGroup(key, ownerRoots, obligations, foreign)
}

// deferredDoneGroup recognizes a literal whose first statement defers its only
// Done call and which contains no goto. Other shapes remain raw.
func deferredDoneGroup(pass *analysis.Pass, lit *ast.FuncLit, resolver *pathResolver) (groupKey, bool) {
	if len(lit.Body.List) == 0 {
		return groupKey{}, false
	}
	first, ok := lit.Body.List[0].(*ast.DeferStmt)
	if !ok || !isSyncMethodCall(pass, first.Call, "WaitGroup", "Done") {
		return groupKey{}, false
	}
	sel, ok := first.Call.Fun.(*ast.SelectorExpr)
	if !ok {
		return groupKey{}, false
	}
	root, path, ok := resolver.resolve(sel.X)
	if !ok || !soleDoneWithoutGoto(pass, lit, first) {
		return groupKey{}, false
	}
	return groupKey{root: root, path: path}, true
}

// soleDoneWithoutGoto requires exactly one Done call across the literal and
// nested literals, with no goto.
func soleDoneWithoutGoto(pass *analysis.Pass, lit *ast.FuncLit, first *ast.DeferStmt) bool {
	ok := true
	ast.Inspect(lit.Body, func(node ast.Node) bool {
		if !ok {
			return false
		}
		switch n := node.(type) {
		case *ast.BranchStmt:
			if n.Tok == token.GOTO {
				ok = false
			}
		case *ast.CallExpr:
			if n != first.Call && isSyncMethodCall(pass, n, "WaitGroup", "Done") {
				ok = false
			}
		}
		return true
	})
	return ok
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

// addCapacity tracks Add units by normalized group and CFG location. Each
// launch spends one unit. Loops and unaccounted Add or Done operations poison
// the affected group; a goto poisons all groups.
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

// dominates requires the Add before every arrival at the launch without a loop
// multiplying the launch. An Add and launch in the same loop pair per iteration.
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
	if addChain[last].block != goChain[last].block || addChain[last].index >= goChain[last].index {
		return false
	}
	for k := last; k < len(goChain); k++ {
		if isLoopStmt(goChain[k].block.List[goChain[k].index]) {
			return false
		}
	}
	return true
}

// isLoopStmt reports whether stmt repeats its body, looking through labels.
func isLoopStmt(stmt ast.Stmt) bool {
	for {
		labeled, ok := stmt.(*ast.LabeledStmt)
		if !ok {
			break
		}
		stmt = labeled.Stmt
	}
	switch stmt.(type) {
	case *ast.ForStmt, *ast.RangeStmt:
		return true
	}
	return false
}

// collectAddCapacity records direct, constant, non-negative Add statements by
// normalized group. Unaccounted counter operations poison their group;
// unresolved receivers and gotos poison all capacity.
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
			c.poisonCounterOps(pass, n.Body, resolver, nil)
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

// scanLaunchedLiteral exempts the recognized first-statement deferred Done.
// Rejected shapes and accepted launches without capacity poison their group.
func (c *addCapacity) scanLaunchedLiteral(pass *analysis.Pass, lit *ast.FuncLit, resolver *pathResolver) {
	if _, ok := deferredDoneGroup(pass, lit, resolver); !ok {
		c.poisonCounterOps(pass, lit.Body, resolver, nil)
		return
	}
	first, _ := lit.Body.List[0].(*ast.DeferStmt)
	c.poisonCounterOps(pass, lit.Body, resolver, first.Call)
}

// poisonCounterOps poisons the group of every WaitGroup Add or Done found
// under node, nested literals included, except the one exempt call (nil
// for none): counter operations the recognized shape does not account for
// run at times the registration model cannot order.
func (c *addCapacity) poisonCounterOps(pass *analysis.Pass, node ast.Node, resolver *pathResolver, exempt *ast.CallExpr) {
	ast.Inspect(node, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && call != exempt {
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

// collectOwnerRoots maps start-body variables to receiver or returned owners.
// Multiple possible owner identities resolve to sentinel index -1 and fail closed.
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
