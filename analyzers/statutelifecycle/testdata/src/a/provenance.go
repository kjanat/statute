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
