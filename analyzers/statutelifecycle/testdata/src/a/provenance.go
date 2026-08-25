package a

import "sync"

// SLC103 provenance fixtures: WaitGroup cleanup evidence counts only for
// lifecycle work launched through the exact same owner-relative group.

// Two groups on one returned owner: a wait on one group cannot discharge
// work launched through the other.
type twoGroupWorker struct{}
type twoGroupRun struct {
	workWG  sync.WaitGroup
	otherWG sync.WaitGroup
}

func (*twoGroupWorker) start() *twoGroupRun { // want `\[SLC103\].*on WaitGroup twoGroupRun\.workWG but stop never waits on that group`
	r := &twoGroupRun{}
	r.workWG.Go(func() {})
	return r
}

func (r *twoGroupRun) stop() { r.otherWG.Wait() }

// Both groups launched, one waited: only the uncovered group is reported.
type halfWaitedWorker struct{}
type halfWaitedRun struct {
	a sync.WaitGroup
	b sync.WaitGroup
}

func (*halfWaitedWorker) start() *halfWaitedRun { // want `\[SLC103\].*on WaitGroup halfWaitedRun\.b but stop never waits on that group`
	r := &halfWaitedRun{}
	r.a.Go(func() {})
	r.b.Go(func() {})
	return r
}

func (r *halfWaitedRun) stop() { r.a.Wait() }

// Nested paths: r.a.wg and r.b.wg end in the same declared field type but
// are different groups.
type nestedGroup struct{ wg sync.WaitGroup }
type nestedWorker struct{}
type nestedRun struct {
	a nestedGroup
	b nestedGroup
}

func (*nestedWorker) start() *nestedRun { // want `\[SLC103\].*on WaitGroup nestedRun\.a\.wg but stop never waits on that group`
	r := &nestedRun{}
	r.a.wg.Go(func() {})
	return r
}

func (r *nestedRun) stop() { r.b.wg.Wait() }

// Matching nested path is clean.
type nestedCleanWorker struct{}
type nestedCleanRun struct {
	a nestedGroup
	b nestedGroup
}

func (*nestedCleanWorker) start() *nestedCleanRun {
	r := &nestedCleanRun{}
	r.a.wg.Go(func() {})
	return r
}

func (r *nestedCleanRun) stop() { r.a.wg.Wait() }

// The same declared field on an unrelated object is not the returned
// owner's wait.
type unrelatedObjectWorker struct{}
type unrelatedObjectRun struct {
	wg    sync.WaitGroup
	other *unrelatedObjectRun
}

func (*unrelatedObjectWorker) start() *unrelatedObjectRun { // want `\[SLC103\].*on WaitGroup unrelatedObjectRun\.wg but stop never waits on that group`
	r := &unrelatedObjectRun{}
	r.wg.Go(func() {})
	return r
}

func (r *unrelatedObjectRun) stop() { r.other.wg.Wait() }

// A matching Wait inside sync.Once.Do counts; a mismatching one does not.
type onceMismatchWorker struct{}
type onceMismatchRun struct {
	workWG  sync.WaitGroup
	otherWG sync.WaitGroup
	once    sync.Once
}

func (*onceMismatchWorker) start() *onceMismatchRun { // want `\[SLC103\].*on WaitGroup onceMismatchRun\.workWG but stop never waits on that group`
	r := &onceMismatchRun{}
	r.workWG.Go(func() {})
	return r
}

func (r *onceMismatchRun) stop() {
	r.once.Do(func() { r.otherWG.Wait() })
}

// Simple aliases resolve on both sides: the launch through a run alias and
// the wait through a group pointer alias are the same group.
type aliasWorker struct{}
type aliasRun struct{ wg sync.WaitGroup }

func (*aliasWorker) start() *aliasRun {
	r := &aliasRun{}
	run := r
	run.wg.Go(func() {})
	return r
}

func (r *aliasRun) stop() {
	wg := &r.wg
	wg.Wait()
}

// A reassigned alias is ambiguous and is not proof: the wait through it
// does not discharge the receiver group.
type reassignedAliasWorker struct{}
type reassignedAliasRun struct {
	wg    sync.WaitGroup
	other *reassignedAliasRun
}

func (*reassignedAliasWorker) start() *reassignedAliasRun { // want `\[SLC103\].*on WaitGroup reassignedAliasRun\.wg but stop never waits on that group`
	r := &reassignedAliasRun{}
	r.wg.Go(func() {})
	return r
}

func (r *reassignedAliasRun) stop() {
	wg := &r.wg
	wg = &r.other.wg
	wg.Wait()
}

// A reassigned root variable may denote different objects at different
// points: the launch ran on the first object, the caller holds the second,
// and stop would wait an empty group. Fails closed as unresolved.
type reassignedRootWorker struct{}
type reassignedRootRun struct{ wg sync.WaitGroup }

func (*reassignedRootWorker) start() *reassignedRootRun { // want `\[SLC103\].*WaitGroup whose provenance cannot be resolved to a lifecycle owner`
	r := &reassignedRootRun{}
	r.wg.Go(func() {})
	r = &reassignedRootRun{}
	return r
}

func (r *reassignedRootRun) stop() { r.wg.Wait() }

// Replacing an intermediate pointer field replaces the storage the path
// names: the launch ran on the first child's group, stop waits the second
// child's, even though both normalize to the same lexical path. A write to
// any prefix of a group path fails closed as unresolved.
type replacedChildGroup struct{ wg sync.WaitGroup }
type replacedChildWorker struct{}
type replacedChildRun struct{ child *replacedChildGroup }

func (*replacedChildWorker) start() *replacedChildRun { // want `\[SLC103\].*WaitGroup whose provenance cannot be resolved to a lifecycle owner`
	r := &replacedChildRun{child: &replacedChildGroup{}}
	r.child.wg.Go(func() {})
	r.child = &replacedChildGroup{}
	return r
}

func (r *replacedChildRun) stop() { r.child.wg.Wait() }

// Replacing the intermediate field through a pointer alias is the same
// storage replacement: the write through *p resolves to the same prefix.
type derefWriteWorker struct{}
type derefWriteRun struct{ child *replacedChildGroup }

func (*derefWriteWorker) start() *derefWriteRun { // want `\[SLC103\].*WaitGroup whose provenance cannot be resolved to a lifecycle owner`
	r := &derefWriteRun{child: &replacedChildGroup{}}
	p := &r.child
	r.child.wg.Go(func() {})
	*p = &replacedChildGroup{}
	return r
}

func (r *derefWriteRun) stop() { r.child.wg.Wait() }

// Letting a field's address escape hands replacement of that storage to
// code the model cannot see; every path below the escaped prefix is
// unresolvable.
func swapChild(**replacedChildGroup) {}

type escapedChildWorker struct{}
type escapedChildRun struct{ child *replacedChildGroup }

func (*escapedChildWorker) start() *escapedChildRun { // want `\[SLC103\].*WaitGroup whose provenance cannot be resolved to a lifecycle owner`
	r := &escapedChildRun{child: &replacedChildGroup{}}
	swapChild(&r.child)
	r.child.wg.Go(func() {})
	return r
}

func (r *escapedChildRun) stop() { r.child.wg.Wait() }

// Escape through an already-created pointer alias is the same handover:
// once the alias leaves the local model as a value, its target storage
// can be replaced by code the model cannot see.
type escapedAliasWorker struct{}
type escapedAliasRun struct{ child *replacedChildGroup }

func (*escapedAliasWorker) start() *escapedAliasRun { // want `\[SLC103\].*WaitGroup whose provenance cannot be resolved to a lifecycle owner`
	r := &escapedAliasRun{child: &replacedChildGroup{}}
	p := &r.child
	r.child.wg.Go(func() {})
	swapChild(p)
	return r
}

func (r *escapedAliasRun) stop() { r.child.wg.Wait() }

// A write to a sibling field replaces nothing along the group path: only a
// prefix write invalidates provenance.
type siblingWriteWorker struct{}
type siblingWriteRun struct {
	wg      sync.WaitGroup
	started bool
}

func (*siblingWriteWorker) start() *siblingWriteRun {
	r := &siblingWriteRun{}
	r.started = true
	r.wg.Go(func() {})
	return r
}

func (r *siblingWriteRun) stop() { r.wg.Wait() }

// A value copy of a group is not an alias: launching through the copy
// registers work on a different WaitGroup than the owner's, so the owner's
// wait proves nothing. The copy is foreign, not the owner's group.
type copiedGroupWorker struct{}
type copiedGroupRun struct{ wg sync.WaitGroup }

func (*copiedGroupWorker) start() *copiedGroupRun { // want `\[SLC103\].*on WaitGroup wg outside its lifecycle owner`
	r := &copiedGroupRun{}
	wg := r.wg //nolint:govet // the copy is the point: it must not alias the owner's group
	wg.Go(func() {})
	return r
}

func (r *copiedGroupRun) stop() { r.wg.Wait() }

// A value copy on the wait side is equally worthless as evidence.
type copiedWaitWorker struct{}
type copiedWaitRun struct{ wg sync.WaitGroup }

func (*copiedWaitWorker) start() *copiedWaitRun { // want `\[SLC103\].*on WaitGroup copiedWaitRun\.wg but stop never waits on that group`
	r := &copiedWaitRun{}
	r.wg.Go(func() {})
	return r
}

func (r *copiedWaitRun) stop() {
	wg := r.wg //nolint:govet // the copy is the point: waiting it joins nothing
	wg.Wait()
}

// The conventional Add(1) + go + defer Done() shape is one obligation on
// that group, discharged by the matching Wait.
type addDoneWorker struct{}
type addDoneRun struct{ wg sync.WaitGroup }

func (*addDoneWorker) start() *addDoneRun {
	r := &addDoneRun{}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
	}()
	return r
}

func (r *addDoneRun) stop() { r.wg.Wait() }

// A Done deferred on a group nobody Adds to is not that shape: the launch
// stays a raw goroutine owing a completion signal the wait cannot supply.
type mismatchedAddDoneWorker struct{}
type mismatchedAddDoneRun struct {
	addWG  sync.WaitGroup
	doneWG sync.WaitGroup
}

func (*mismatchedAddDoneWorker) start() *mismatchedAddDoneRun { // want `\[SLC103\].*launches 1 lifecycle goroutine.*waits for only 0`
	r := &mismatchedAddDoneRun{}
	r.addWG.Add(1)
	go func() {
		defer r.doneWG.Done()
	}()
	return r
}

func (r *mismatchedAddDoneRun) stop() {
	r.addWG.Wait()
	r.doneWG.Wait()
}

// One Add(1) cannot vouch for two deferred-Done goroutines: only one was
// registered, so whichever finishes first releases Wait while the other
// still runs. The second launch stays a raw obligation.
type doubleDoneWorker struct{}
type doubleDoneRun struct{ wg sync.WaitGroup }

func (*doubleDoneWorker) start() *doubleDoneRun { // want `\[SLC103\].*launches 1 lifecycle goroutine.*waits for only 0`
	r := &doubleDoneRun{}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
	}()
	go func() {
		defer r.wg.Done()
	}()
	return r
}

func (r *doubleDoneRun) stop() { r.wg.Wait() }

// Add(0) registers nothing; the deferred Done cannot spend capacity that
// was never added, so the launch stays raw.
type addZeroWorker struct{}
type addZeroRun struct{ wg sync.WaitGroup }

func (*addZeroWorker) start() *addZeroRun { // want `\[SLC103\].*launches 1 lifecycle goroutine.*waits for only 0`
	r := &addZeroRun{}
	r.wg.Add(0)
	go func() {
		defer r.wg.Done()
	}()
	return r
}

func (r *addZeroRun) stop() { r.wg.Wait() }

// Registration must precede the launch: an Add after the go statement
// cannot prove the goroutine was registered when it started.
type addAfterGoWorker struct{}
type addAfterGoRun struct{ wg sync.WaitGroup }

func (*addAfterGoWorker) start() *addAfterGoRun { // want `\[SLC103\].*launches 1 lifecycle goroutine.*waits for only 0`
	r := &addAfterGoRun{}
	go func() {
		defer r.wg.Done()
	}()
	r.wg.Add(1)
	return r
}

func (r *addAfterGoRun) stop() { r.wg.Wait() }

// Registration must dominate the launch, not merely precede it in the
// source: an Add inside a conditional branch may never have executed.
type conditionalAddWorker struct{}
type conditionalAddRun struct{ wg sync.WaitGroup }

func (*conditionalAddWorker) start(cond bool) *conditionalAddRun { // want `\[SLC103\].*launches 1 lifecycle goroutine.*waits for only 0`
	r := &conditionalAddRun{}
	if cond {
		r.wg.Add(1)
	}
	go func() {
		defer r.wg.Done()
	}()
	return r
}

func (r *conditionalAddRun) stop() { r.wg.Wait() }

// A deferred Add occupies an earlier source position but executes only at
// return: it registers nothing before the launch.
type deferredAddWorker struct{}
type deferredAddRun struct{ wg sync.WaitGroup }

func (*deferredAddWorker) start() *deferredAddRun { // want `\[SLC103\].*launches 1 lifecycle goroutine.*waits for only 0`
	r := &deferredAddRun{}
	defer r.wg.Add(1)
	go func() {
		defer r.wg.Done()
	}()
	return r
}

func (r *deferredAddRun) stop() { r.wg.Wait() }

// A Done in the start body consumes registration the model granted: the
// counter is back to zero before the launch, so the capacity is poisoned
// and the goroutine stays raw.
type depletedAddWorker struct{}
type depletedAddRun struct{ wg sync.WaitGroup }

func (*depletedAddWorker) start() *depletedAddRun { // want `\[SLC103\].*launches 1 lifecycle goroutine.*waits for only 0`
	r := &depletedAddRun{}
	r.wg.Add(1)
	r.wg.Done()
	go func() {
		defer r.wg.Done()
	}()
	return r
}

func (r *depletedAddRun) stop() { r.wg.Wait() }

// Block ordering is dominance only for structured control flow: a goto
// can land between the Add and the launch, so it disables registration
// for the whole body and the goroutine stays raw.
type gotoAddWorker struct{}
type gotoAddRun struct{ wg sync.WaitGroup }

func (*gotoAddWorker) start() *gotoAddRun { // want `\[SLC103\].*launches 1 lifecycle goroutine.*waits for only 0`
	r := &gotoAddRun{}
	goto launch
	r.wg.Add(1)
launch:
	go func() {
		defer r.wg.Done()
	}()
	return r
}

func (r *gotoAddRun) stop() { r.wg.Wait() }

// A Done inside a synchronously invoked function literal consumes the
// registration before the launch just as a direct Done does: counter
// operations do not become invisible behind a literal.
type iifeDoneWorker struct{}
type iifeDoneRun struct{ wg sync.WaitGroup }

func (*iifeDoneWorker) start() *iifeDoneRun { // want `\[SLC103\].*launches 1 lifecycle goroutine.*waits for only 0`
	r := &iifeDoneRun{}
	r.wg.Add(1)
	func() {
		r.wg.Done()
	}()
	go func() {
		defer r.wg.Done()
	}()
	return r
}

func (r *iifeDoneRun) stop() { r.wg.Wait() }

// The same through a deferred literal: it mutates the counter at return,
// a time the registration model cannot order against the launch.
type deferLitDoneWorker struct{}
type deferLitDoneRun struct{ wg sync.WaitGroup }

func (*deferLitDoneWorker) start() *deferLitDoneRun { // want `\[SLC103\].*launches 1 lifecycle goroutine.*waits for only 0`
	r := &deferLitDoneRun{}
	r.wg.Add(1)
	defer func() {
		r.wg.Done()
	}()
	go func() {
		defer r.wg.Done()
	}()
	return r
}

func (r *deferLitDoneRun) stop() { r.wg.Wait() }

// A loop between the registration and the launch multiplies the launch:
// one Add(1) outside the loop cannot vouch for a go statement the runtime
// executes ten times, so the launch stays raw.
type loopLaunchWorker struct{}
type loopLaunchRun struct{ wg sync.WaitGroup }

func (*loopLaunchWorker) start() *loopLaunchRun { // want `\[SLC103\].*launches 1 lifecycle goroutine.*waits for only 0`
	r := &loopLaunchRun{}
	r.wg.Add(1)
	for range 10 {
		go func() {
			defer r.wg.Done()
		}()
	}
	return r
}

func (r *loopLaunchRun) stop() { r.wg.Wait() }

// Registration and launch inside the same loop body pair one-to-one per
// iteration and stay clean.
type loopPairedWorker struct{}
type loopPairedRun struct{ wg sync.WaitGroup }

func (*loopPairedWorker) start() *loopPairedRun {
	r := &loopPairedRun{}
	for range 10 {
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
		}()
	}
	return r
}

func (r *loopPairedRun) stop() { r.wg.Wait() }

// Two deferred Dones on one group inside one launched literal are not the
// conventional shape: the goroutine consumes two registrations for one
// launch, releasing Wait early and driving the counter negative. The
// launch stays raw.
type doubleDeferredDoneWorker struct{}
type doubleDeferredDoneRun struct{ wg sync.WaitGroup }

func (*doubleDeferredDoneWorker) start() *doubleDeferredDoneRun { // want `\[SLC103\].*launches 1 lifecycle goroutine.*waits for only 0`
	r := &doubleDeferredDoneRun{}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer r.wg.Done()
	}()
	return r
}

func (r *doubleDeferredDoneRun) stop() { r.wg.Wait() }

// One syntactic defer inside a loop registers per iteration: the first
// Done releases Wait while the goroutine still runs, then the second
// drives the counter negative. Not the conventional shape; stays raw.
type loopDeferDoneWorker struct{}
type loopDeferDoneRun struct{ wg sync.WaitGroup }

func (*loopDeferDoneWorker) start() *loopDeferDoneRun { // want `\[SLC103\].*launches 1 lifecycle goroutine.*waits for only 0`
	r := &loopDeferDoneRun{}
	r.wg.Add(1)
	go func() {
		for range 2 {
			defer r.wg.Done()
		}
	}()
	return r
}

func (r *loopDeferDoneRun) stop() { r.wg.Wait() }

// A defer under a conditional may execute zero times, leaving the
// registration unclaimed: not proof of one Done per launch.
type conditionalDeferDoneWorker struct{}
type conditionalDeferDoneRun struct{ wg sync.WaitGroup }

func (*conditionalDeferDoneWorker) start(cond bool) *conditionalDeferDoneRun { // want `\[SLC103\].*launches 1 lifecycle goroutine.*waits for only 0`
	r := &conditionalDeferDoneRun{}
	r.wg.Add(1)
	go func() {
		if cond {
			defer r.wg.Done()
		}
	}()
	return r
}

func (r *conditionalDeferDoneRun) stop() { r.wg.Wait() }

// A goto inside the launched literal can revisit the registration, so the
// literal is not the conventional shape even with the defer first.
type gotoDeferDoneWorker struct{}
type gotoDeferDoneRun struct{ wg sync.WaitGroup }

func (*gotoDeferDoneWorker) start() *gotoDeferDoneRun { // want `\[SLC103\].*launches 1 lifecycle goroutine.*waits for only 0`
	r := &gotoDeferDoneRun{}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		goto skip
	skip:
		_ = r
	}()
	return r
}

func (r *gotoDeferDoneRun) stop() { r.wg.Wait() }

// A deferred Done preceded by other statements can be skipped by an early
// return before its registration: only the first statement provably
// executes once per launch.
type lateDeferDoneWorker struct{}
type lateDeferDoneRun struct {
	wg    sync.WaitGroup
	ready bool
}

func (*lateDeferDoneWorker) start() *lateDeferDoneRun { // want `\[SLC103\].*launches 1 lifecycle goroutine.*waits for only 0`
	r := &lateDeferDoneRun{}
	r.wg.Add(1)
	go func() {
		if !r.ready {
			return
		}
		defer r.wg.Done()
	}()
	return r
}

func (r *lateDeferDoneRun) stop() { r.wg.Wait() }

// A rejected literal's Done poisons the group's capacity: the raw first
// goroutine can perform the Done that releases Wait while the accepted
// second goroutine — to which the analyzer would otherwise attribute the
// sole Add(1) — is still running. Both launches stay raw.
type contaminatedCapacityWorker struct{}
type contaminatedCapacityRun struct {
	wg      sync.WaitGroup
	rawDone chan struct{}
	block   chan struct{}
}

func (*contaminatedCapacityWorker) start(cond bool) *contaminatedCapacityRun { // want `\[SLC103\].*launches 2 lifecycle goroutine.*waits for only 1`
	r := &contaminatedCapacityRun{rawDone: make(chan struct{}), block: make(chan struct{})}
	r.wg.Add(1)
	go func() {
		if cond {
			defer r.wg.Done()
		}
		close(r.rawDone)
	}()
	go func() {
		defer r.wg.Done()
		<-r.block
	}()
	return r
}

func (r *contaminatedCapacityRun) stop() {
	<-r.rawDone
	r.wg.Wait()
}

// Two accepted launches each spending their own registration stay clean:
// poisoning applies to unaccounted operations, not to the shape itself.
type pairedAddDoneWorker struct{}
type pairedAddDoneRun struct{ wg sync.WaitGroup }

func (*pairedAddDoneWorker) start() *pairedAddDoneRun {
	r := &pairedAddDoneRun{}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
	}()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
	}()
	return r
}

func (r *pairedAddDoneRun) stop() { r.wg.Wait() }

// A perfectly shaped launch that no registration dominates is still raw,
// and its Done poisons the group: at runtime that Done consumes the later
// Add(1), releasing Wait while the second goroutine is still blocked.
type orphanedShapeWorker struct{}
type orphanedShapeRun struct {
	wg      sync.WaitGroup
	rawDone chan struct{}
	release chan struct{}
	block   chan struct{}
}

func (*orphanedShapeWorker) start() *orphanedShapeRun { // want `\[SLC103\].*launches 2 lifecycle goroutine.*waits for only 1`
	r := &orphanedShapeRun{rawDone: make(chan struct{}), release: make(chan struct{}), block: make(chan struct{})}
	go func() {
		defer r.wg.Done()
		<-r.release
		close(r.rawDone)
	}()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		<-r.block
	}()
	close(r.release)
	return r
}

func (r *orphanedShapeRun) stop() {
	<-r.rawDone
	r.wg.Wait()
}

// An Add in an enclosing block dominates a launch nested below it: every
// structured path to the go statement passes the registration first.
type dominatingAddWorker struct{}
type dominatingAddRun struct{ wg sync.WaitGroup }

func (*dominatingAddWorker) start(cond bool) *dominatingAddRun {
	r := &dominatingAddRun{}
	r.wg.Add(1)
	if cond {
		go func() {
			defer r.wg.Done()
		}()
	}
	return r
}

func (r *dominatingAddRun) stop() { r.wg.Wait() }

// Mixed raw go and WaitGroup work: the group wait alone cannot discharge
// the raw goroutine, and the channel join alone cannot discharge the group.
type mixedWaitOnlyWorker struct{}
type mixedWaitOnlyRun struct {
	wg   sync.WaitGroup
	done chan struct{}
}

func (*mixedWaitOnlyWorker) start() *mixedWaitOnlyRun { // want `\[SLC103\].*launches 1 lifecycle goroutine.*waits for only 0`
	r := &mixedWaitOnlyRun{done: make(chan struct{})}
	r.wg.Go(func() {})
	go func() { close(r.done) }()
	return r
}

func (r *mixedWaitOnlyRun) stop() { r.wg.Wait() }

type mixedJoinOnlyWorker struct{}
type mixedJoinOnlyRun struct {
	wg   sync.WaitGroup
	done chan struct{}
}

func (*mixedJoinOnlyWorker) start() *mixedJoinOnlyRun { // want `\[SLC103\].*on WaitGroup mixedJoinOnlyRun\.wg but stop never waits on that group`
	r := &mixedJoinOnlyRun{done: make(chan struct{})}
	r.wg.Go(func() {})
	go func() { close(r.done) }()
	return r
}

func (r *mixedJoinOnlyRun) stop() { <-r.done }

// Both obligations discharged is clean.
type mixedCleanWorker struct{}
type mixedCleanRun struct {
	wg   sync.WaitGroup
	done chan struct{}
}

func (*mixedCleanWorker) start() *mixedCleanRun {
	r := &mixedCleanRun{done: make(chan struct{})}
	r.wg.Go(func() {})
	go func() { close(r.done) }()
	return r
}

func (r *mixedCleanRun) stop() {
	<-r.done
	r.wg.Wait()
}

// Two distinct variables returned at one result position are ambiguous:
// the caller may hold either object, so a launch through one of them can
// never be discharged by receiver-rooted evidence that may run on the
// other. Fails closed as unresolved provenance.
type multiReturnWorker struct{}
type multiReturnRun struct{ wg sync.WaitGroup }

func (*multiReturnWorker) start(cond bool) *multiReturnRun { // want `\[SLC103\].*WaitGroup whose provenance cannot be resolved to a lifecycle owner`
	a := &multiReturnRun{}
	b := &multiReturnRun{}
	a.wg.Go(func() {})
	if cond {
		return b
	}
	return a
}

func (r *multiReturnRun) stop() { r.wg.Wait() }

// Several return statements of the same variable are one unambiguous
// owner root: plural returns are not plural owners.
type multiReturnSameWorker struct{}
type multiReturnSameRun struct{ wg sync.WaitGroup }

func (*multiReturnSameWorker) start(cond bool) *multiReturnSameRun {
	r := &multiReturnSameRun{}
	r.wg.Go(func() {})
	if cond {
		return r
	}
	return r
}

func (r *multiReturnSameRun) stop() { r.wg.Wait() }

// Two returned lifecycle owners are evaluated independently: only the
// owner whose group is never waited reports.
type dualOwnerWorker struct{}
type dualOwnerA struct{ wg sync.WaitGroup }
type dualOwnerB struct{ wg sync.WaitGroup }

func (*dualOwnerWorker) start() (*dualOwnerA, *dualOwnerB) { // want `\[SLC103\].*on WaitGroup dualOwnerB\.wg but stop never waits on that group`
	a := &dualOwnerA{}
	b := &dualOwnerB{}
	a.wg.Go(func() {})
	b.wg.Go(func() {})
	return a, b
}

func (a *dualOwnerA) stop() { a.wg.Wait() }

func (b *dualOwnerB) stop() {}
