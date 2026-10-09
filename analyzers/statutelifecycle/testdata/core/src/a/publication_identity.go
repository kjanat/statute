package a

import "net"

func siblingInstanceStart(a, other *bracketAttempt) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	other.serveEarly()
	_, err := bind()
	return err
}

func reassignedRootStart(a, other *bracketAttempt) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	a = other
	a.serveEarly()
	_, err := bind()
	return err
}

func replacedServerStart(a, other *bracketAttempt) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	a.serveEarly()
	a.hs = other.hs
	_, err := bind()
	return err
}

func replacedSignalStart(a, other *bracketAttempt) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	a.serveEarly()
	a.done = other.done
	_, err := bind()
	return err
}

func replacedAncestorStart(a, other *bracketNestedAttempt, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	a.serveEarly(ln)
	a.bound = other.bound
	_, err := bind()
	return err
}

func replacedAliasStart(a, other *bracketAttempt) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	alias := a
	alias.serveEarly()
	alias.hs = other.hs
	_, err := bind()
	return err
}

func (a *bracketAttempt) serveAndReplace(other *bracketAttempt) {
	old := a.hs
	go func() {
		defer close(a.done)
		if err := old.Serve(a.ln); err != nil {
			_ = err
		}
	}()
	a.hs = other.hs
}

func replacedInHelperStart(a, other *bracketAttempt) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	a.serveAndReplace(other)
	_, err := bind()
	return err
}

type siblingAttempt struct {
	one, two *bracketBound
	finished bool
}

func (a *siblingAttempt) rollback() error { return a.one.rollback() }

func (a *siblingAttempt) serveTwo(ln net.Listener) {
	go func() {
		defer close(a.two.done)
		if err := a.two.hs.Serve(ln); err != nil {
			_ = err
		}
	}()
}

func siblingFieldStart(a *siblingAttempt, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	a.serveTwo(ln)
	_, err := bind()
	return err
}

func (a *siblingAttempt) freshServe(ln net.Listener) {
	b := &bracketBound{done: make(chan struct{})}
	a.one = b
	go func() {
		defer close(b.done)
		if err := b.hs.Serve(ln); err != nil {
			_ = err
		}
	}()
}

func freshTransferStart(a *siblingAttempt, ln net.Listener) error {
	defer a.rollback()
	a.freshServe(ln)
	a.finished = true // Sibling metadata does not invalidate the owned server.
	_, err := bind()
	return err
}

func transferThenReplaceStart(a, other *siblingAttempt, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	a.freshServe(ln)
	a.one = other.one
	_, err := bind()
	return err
}

func (a *siblingAttempt) conditionalTransfer(ln net.Listener, transfer bool) {
	b := &bracketBound{done: make(chan struct{})}
	if transfer {
		a.one = b
	}
	go func() {
		defer close(b.done)
		if err := b.hs.Serve(ln); err != nil {
			_ = err
		}
	}()
}

func conditionalTransferStart(a *siblingAttempt, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	a.conditionalTransfer(ln, false)
	_, err := bind()
	return err
}

func (a *siblingAttempt) lateTransfer(ln net.Listener) {
	b := &bracketBound{done: make(chan struct{})}
	go func() {
		defer close(b.done)
		if err := b.hs.Serve(ln); err != nil {
			_ = err
		}
	}()
	a.one = b
}

func lateTransferStart(a *siblingAttempt, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	a.lateTransfer(ln)
	_, err := bind()
	return err
}

func stableAliasStart(a *bracketAttempt) error {
	defer a.rollback()
	alias := a
	alias.serveEarly()
	_, err := bind()
	return err
}

func (a *bracketAttempt) shadowedCompletion() {
	close := func(chan struct{}) {}
	go func() {
		defer close(a.done)
		if err := a.hs.Serve(a.ln); err != nil {
			_ = err
		}
	}()
}

func shadowedCompletionStart(a *bracketAttempt) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	a.shadowedCompletion()
	_, err := bind()
	return err
}

func replaceLater(a, other *bracketAttempt) { a.hs = other.hs }

func mutationOnlyHelperStart(a, other *bracketAttempt) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	a.serveEarly()
	replaceLater(a, other)
	_, err := bind()
	return err
}

func overwriteOwner(a, other *bracketAttempt) { *a = *other }

func mutationThroughAddressStart(other *bracketAttempt) error { // want `\[SLC100\].*publish serving before a later error return`
	var a bracketAttempt
	defer a.rollback()
	a.serveEarly()
	overwriteOwner(&a, other)
	_, err := bind()
	return err
}

func updateSibling(a *siblingAttempt) { a.finished = true }

func boundedAddressStart(ln net.Listener) error {
	var a siblingAttempt
	defer a.rollback()
	a.freshServe(ln)
	updateSibling(&a)
	_, err := bind()
	return err
}

var escapedAttempt *bracketAttempt

func retainOwner(a *bracketAttempt) { escapedAttempt = a }

func escapedOwnerStart(a *bracketAttempt) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	a.serveEarly()
	retainOwner(a)
	_, err := bind()
	return err
}

func replacingListener(a, other *bracketAttempt) net.Listener {
	a.hs = other.hs
	return a.ln
}

func (a *bracketAttempt) argumentMutation(other *bracketAttempt) {
	go func() {
		defer close(a.done)
		if err := a.hs.Serve(replacingListener(a, other)); err != nil {
			_ = err
		}
	}()
}

func serveArgumentMutationStart(a, other *bracketAttempt) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	a.argumentMutation(other)
	_, err := bind()
	return err
}

func literalMutationStart(a, other *bracketAttempt) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	a.serveEarly()
	func(p *bracketAttempt) { p.hs = other.hs }(a)
	_, err := bind()
	return err
}

func literalPublicationStart(a *bracketAttempt) error {
	defer a.rollback()
	func(p *bracketAttempt) { p.serveEarly() }(a)
	_, err := bind()
	return err
}

func literalSiblingPublicationStart(a, other *bracketAttempt) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	func(p *bracketAttempt) { p.serveEarly() }(other)
	_, err := bind()
	return err
}

type publicationObserver struct{}

func (o *publicationObserver) observe() {}

func replacingObserver(a, other *bracketAttempt) *publicationObserver {
	a.hs = other.hs
	return &publicationObserver{}
}

func receiverMutationStart(a, other *bracketAttempt) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	a.serveEarly()
	replacingObserver(a, other).observe()
	_, err := bind()
	return err
}

var retainCompletion func(chan struct{})

func escapedCompletionStart(a *bracketAttempt) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	a.serveEarly()
	retainCompletion(a.done)
	_, err := bind()
	return err
}

func escapedCompletionAliasStart(a *bracketAttempt) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	a.serveEarly()
	done := a.done
	retainCompletion(done)
	_, err := bind()
	return err
}

func prematureCompletionStart(a *bracketAttempt) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	a.serveEarly()
	close(a.done)
	_, err := bind()
	return err
}

func retainVariadic(owners ...*bracketAttempt) { escapedAttempt = owners[0] }

func variadicEscapeStart(a *bracketAttempt) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	a.serveEarly()
	retainVariadic(a)
	_, err := bind()
	return err
}

func replaceAndRebind(p, other *bracketAttempt) {
	p.hs = other.hs
	p = other
	_ = p
}

func reboundFormalStart(a, other *bracketAttempt) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	a.serveEarly()
	replaceAndRebind(a, other)
	_, err := bind()
	return err
}

func (a *bracketAttempt) replace(other *bracketAttempt) { a.hs = other.hs }

func storedMethodStart(a, other *bracketAttempt) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	a.serveEarly()
	replace := a.replace
	replace(other)
	_, err := bind()
	return err
}

func storedClosureStart(a, other *bracketAttempt) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	a.serveEarly()
	replace := func() { replaceLater(a, other) }
	replace()
	_, err := bind()
	return err
}

func invokeOwner(fn func(*bracketAttempt), other *bracketAttempt) { fn(other) }

func passedMethodStart(a, other *bracketAttempt) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	a.serveEarly()
	invokeOwner(a.replace, other)
	_, err := bind()
	return err
}

func invokeClosure(fn func()) { fn() }

func passedClosureStart(a, other *bracketAttempt) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	a.serveEarly()
	invokeClosure(func() { replaceLater(a, other) })
	_, err := bind()
	return err
}

func consumeOwners(owners []*bracketAttempt) { escapedAttempt = owners[0] }

func passedAggregateStart(a *bracketAttempt) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	a.serveEarly()
	consumeOwners([]*bracketAttempt{a})
	_, err := bind()
	return err
}
