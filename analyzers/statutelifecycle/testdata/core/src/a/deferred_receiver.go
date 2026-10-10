package a

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
)

func launchBound(b *bracketBound, ln net.Listener) {
	go func() {
		defer close(b.done)
		if err := b.hs.Serve(ln); err != nil {
			_ = err
		}
	}()
}

func selectedSiblingInlineStart(a *siblingAttempt, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.two.rollback()
	go func() {
		defer close(a.one.done)
		if err := a.one.hs.Serve(ln); err != nil {
			_ = err
		}
	}()
	_, err := bind()
	return err
}

func selectedSiblingHelperStart(a *siblingAttempt, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.two.rollback()
	launchBound(a.one, ln)
	_, err := bind()
	return err
}

func selectedSiblingLiteralStart(a *siblingAttempt, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer func() { _ = a.two.rollback() }()
	launchBound(a.one, ln)
	_, err := bind()
	return err
}

func selectedOwnFieldStart(a *siblingAttempt, ln net.Listener) error {
	defer a.two.rollback()
	launchBound(a.two, ln)
	_, err := bind()
	return err
}

func selectedOwnInlineStart(a *siblingAttempt, ln net.Listener) error {
	defer a.two.rollback()
	go func() {
		defer close(a.two.done)
		if err := a.two.hs.Serve(ln); err != nil {
			_ = err
		}
	}()
	_, err := bind()
	return err
}

func selectedPartiallyCoveredLiteralStart(a *siblingAttempt, hs *http.Server, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.one.rollback()
	go func() {
		defer close(a.one.done)
		if err := a.one.hs.Serve(ln); err != nil {
			_ = err
		}
		if err := hs.Serve(ln); err != nil {
			_ = err
		}
	}()
	_, err := bind()
	return err
}

type pairedBound struct {
	one, two *http.Server
	done     chan struct{}
}

func (b *pairedBound) rollback() error {
	first := b.one.Close()
	second := b.two.Close()
	<-b.done
	return errors.Join(first, second)
}

func selectedFullyCoveredLiteralStart(b *pairedBound, ln net.Listener) error {
	defer b.rollback()
	go func() {
		defer close(b.done)
		if err := b.one.Serve(ln); err != nil {
			_ = err
		}
		if err := b.two.Serve(ln); err != nil {
			_ = err
		}
	}()
	_, err := bind()
	return err
}

type fieldOnlyAttempt struct{ bound *bracketBound }

func selectedWithoutRootMethodStart(a *fieldOnlyAttempt, ln net.Listener) error {
	defer a.bound.rollback()
	launchBound(a.bound, ln)
	_, err := bind()
	return err
}

func selectedStableAliasStart(a *siblingAttempt, ln net.Listener) error {
	b := a.two
	defer b.rollback()
	launchBound(a.two, ln)
	_, err := bind()
	return err
}

func selectedParenthesesStart(a *siblingAttempt, ln net.Listener) error {
	defer a.two.rollback() // TestAnalyzer restores parentheses in the typed AST.
	launchBound(a.two, ln)
	_, err := bind()
	return err
}

func selectedLiteralParameterStart(a *siblingAttempt, ln net.Listener) error {
	defer func(b *bracketBound) { _ = b.rollback() }(a.two)
	launchBound(a.two, ln)
	_, err := bind()
	return err
}

func selectedLiteralSiblingStart(a *siblingAttempt, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer func(b *bracketBound) { _ = b.rollback() }(a.two)
	launchBound(a.one, ln)
	_, err := bind()
	return err
}

type promotedAttempt struct{ *bracketBound }

func selectedPromotedStart(a *promotedAttempt, ln net.Listener) error {
	defer a.rollback()
	go func() {
		defer close(a.done)
		if err := a.hs.Serve(ln); err != nil {
			_ = err
		}
	}()
	_, err := bind()
	return err
}

type promotedMiddle struct{ *promotedAttempt }

func selectedNestedPromotionStart(a *promotedMiddle, ln net.Listener) error {
	defer a.rollback()
	launchBound(a.bracketBound, ln)
	_, err := bind()
	return err
}

func lateSiblingDeferStart(a *siblingAttempt, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.two.rollback()
	launchBound(a.one, ln)
	defer a.one.rollback()
	_, err := bind()
	return err
}

func separateSiblingDefersStart(a *siblingAttempt, ln net.Listener) error {
	defer a.one.rollback()
	launchBound(a.one, ln)
	defer a.two.rollback()
	launchBound(a.two, ln)
	_, err := bind()
	return err
}

func conditionalSiblingDeferStart(a *siblingAttempt, ln net.Listener, other bool) error { // want `\[SLC100\].*publish serving before a later error return`
	if other {
		defer a.two.rollback()
	} else {
		defer a.one.rollback()
	}
	launchBound(a.one, ln)
	_, err := bind()
	return err
}

func inertNestedDeferStart(a *siblingAttempt, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	_ = func() { defer a.one.rollback() }
	launchBound(a.one, ln)
	_, err := bind()
	return err
}

func inertNestedRollbackStart(a *siblingAttempt, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer func() { _ = func() { _ = a.one.rollback() } }()
	launchBound(a.one, ln)
	_, err := bind()
	return err
}

func selectedMethodExpressionStart(a *siblingAttempt, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer (*bracketBound).rollback(a.one)
	launchBound(a.one, ln)
	_, err := bind()
	return err
}

func selectedFunctionValueStart(a *siblingAttempt, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	rollback := a.one.rollback
	defer rollback()
	launchBound(a.one, ln)
	_, err := bind()
	return err
}

func returnedBound(b *bracketBound) *bracketBound { return b }

func selectedCallReceiverStart(a *siblingAttempt, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer returnedBound(a.one).rollback()
	launchBound(a.one, ln)
	_, err := bind()
	return err
}

func selectedAggregateReceiverStart(a *siblingAttempt, bounds []*bracketBound, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer bounds[0].rollback()
	launchBound(a.one, ln)
	_, err := bind()
	return err
}

func selectedAncestorMutationStart(a, other *siblingAttempt, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.two.rollback()
	launchBound(a.two, ln)
	a.two = other.two
	_, err := bind()
	return err
}

func selectedSiblingMutationStart(a, other *siblingAttempt, ln net.Listener) error {
	defer a.two.rollback()
	launchBound(a.two, ln)
	a.one = other.one
	_, err := bind()
	return err
}

var escapedBound *bracketBound

func retainBound(b *bracketBound) { escapedBound = b }

func selectedEscapeStart(a *siblingAttempt, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.two.rollback()
	launchBound(a.two, ln)
	retainBound(a.two)
	_, err := bind()
	return err
}

func selectedSiblingEscapeStart(a *siblingAttempt, ln net.Listener) error {
	defer a.two.rollback()
	launchBound(a.two, ln)
	retainBound(a.one)
	_, err := bind()
	return err
}

type parameterizedRollbackAttempt struct{ one, two *bracketBound }

func (a *parameterizedRollbackAttempt) rollback(ignored error) error {
	return a.two.rollback()
}

func evaluatedDeferArgumentStart(a *parameterizedRollbackAttempt, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback(a.one.rollback())
	launchBound(a.one, ln)
	_, err := bind()
	return err
}

func evaluatedLiteralArgumentStart(a *siblingAttempt, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer func(ignored error) { _ = a.two.rollback() }(a.one.rollback())
	launchBound(a.one, ln)
	_, err := bind()
	return err
}

func deferredLiteralBodyArgumentStart(a *parameterizedRollbackAttempt, ln net.Listener) error {
	defer func() { _ = a.rollback(a.one.rollback()) }()
	launchBound(a.one, ln)
	_, err := bind()
	return err
}

func (b *bracketBound) launchOther(other *bracketBound, ln net.Listener) {
	launchBound(other, ln)
}

func methodExpressionExplicitArgumentStart(a *siblingAttempt, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.one.rollback()
	(*bracketBound).launchOther(a.one, a.two, ln)
	_, err := bind()
	return err
}

func supportedReceiverExplicitArgumentStart(a *siblingAttempt, ln net.Listener) error {
	defer a.one.rollback()
	a.two.launchOther(a.one, ln)
	_, err := bind()
	return err
}

func returnedReceiverExplicitArgumentStart(a *siblingAttempt, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.one.rollback()
	returnedBound(a.two).launchOther(a.one, ln)
	_, err := bind()
	return err
}

func aggregateReceiverExplicitArgumentStart(a *siblingAttempt, bounds []*bracketBound, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.one.rollback()
	bounds[0].launchOther(a.one, ln)
	_, err := bind()
	return err
}

func launchMethodExpression(a *siblingAttempt, ln net.Listener) {
	(*bracketBound).launchOther(a.one, a.two, ln)
}

func transitiveMethodExpressionStart(a *siblingAttempt, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.one.rollback()
	launchMethodExpression(a, ln)
	_, err := bind()
	return err
}

func (b *bracketBound) clearOther(target *bracketBound) {
	target.hs = nil
}

func mutationMethodExpressionStart(a *siblingAttempt, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.two.rollback()
	launchBound(a.two, ln)
	(*bracketBound).clearOther(a.one, a.two)
	_, err := bind()
	return err
}

func clearMethodExpression(a *siblingAttempt) {
	(*bracketBound).clearOther(a.one, a.two)
}

func transitiveMutationMethodExpressionStart(a *siblingAttempt, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.two.rollback()
	launchBound(a.two, ln)
	clearMethodExpression(a)
	_, err := bind()
	return err
}

type copiedServerAttempt struct {
	hs   http.Server
	done chan struct{}
}

func (a copiedServerAttempt) rollback() error {
	err := a.hs.Close()
	<-a.done
	return err
}

func copiedServerReceiverStart(a *copiedServerAttempt, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	go func() {
		defer close(a.done)
		if err := a.hs.Serve(ln); err != nil {
			_ = err
		}
	}()
	_, err := bind()
	return err
}

type copiedReferenceAttempt struct {
	hs   *http.Server
	done chan struct{}
}

func (a copiedReferenceAttempt) rollback() error {
	err := a.hs.Close()
	<-a.done
	return err
}

func copiedReferenceReceiverStart(a *copiedReferenceAttempt, ln net.Listener) error {
	defer a.rollback()
	go func() {
		defer close(a.done)
		if err := a.hs.Serve(ln); err != nil {
			_ = err
		}
	}()
	_, err := bind()
	return err
}

type copiedGroupAttempt struct {
	hs *http.Server
	wg sync.WaitGroup
}

func (a copiedGroupAttempt) rollback() error {
	err := a.hs.Close()
	a.wg.Wait()
	return err
}

func copiedGroupReceiverStart(a *copiedGroupAttempt, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		if err := a.hs.Serve(ln); err != nil {
			_ = err
		}
	}()
	_, err := bind()
	return err
}

type pointerGroupAttempt copiedGroupAttempt

func (a *pointerGroupAttempt) rollback() error {
	err := a.hs.Close()
	a.wg.Wait()
	return err
}

func pointerGroupReceiverStart(a *pointerGroupAttempt, ln net.Listener) error {
	defer a.rollback()
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		if err := a.hs.Serve(ln); err != nil {
			_ = err
		}
	}()
	_, err := bind()
	return err
}

func replaceGroup(a *pointerGroupAttempt) int {
	a.wg = sync.WaitGroup{}
	return 1
}

func groupAddOperandStart(a *pointerGroupAttempt, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	a.wg.Add(replaceGroup(a))
	go func() {
		defer a.wg.Done()
		if err := a.hs.Serve(ln); err != nil {
			_ = err
		}
	}()
	_, err := bind()
	return err
}

type customAddOwner struct{ bound *bracketBound }

func (a *customAddOwner) Add(int) { a.bound.hs = nil }

func customAddStart(a *customAddOwner, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.bound.rollback()
	launchBound(a.bound, ln)
	a.Add(1)
	_, err := bind()
	return err
}

func launchCopiedServer(a *copiedServerAttempt, ln net.Listener) {
	go func() {
		defer close(a.done)
		if err := a.hs.Serve(ln); err != nil {
			_ = err
		}
	}()
}

type copiedReceiverWrapper struct{ bound *copiedServerAttempt }

func (a *copiedReceiverWrapper) rollback() error { return a.bound.rollback() }

func transitiveCopiedReceiverStart(a *copiedReceiverWrapper, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	launchCopiedServer(a.bound, ln)
	_, err := bind()
	return err
}

func rollbackCopiedValue(a copiedServerAttempt) error {
	err := a.hs.Close()
	<-a.done
	return err
}

type copiedArgumentWrapper struct{ bound *copiedServerAttempt }

func (a *copiedArgumentWrapper) rollback() error { return rollbackCopiedValue(*a.bound) }

func copiedHelperArgumentStart(a *copiedArgumentWrapper, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	launchCopiedServer(a.bound, ln)
	_, err := bind()
	return err
}

type copiedLiteralWrapper struct{ bound *copiedServerAttempt }

func (a *copiedLiteralWrapper) rollback() error {
	return func(b copiedServerAttempt) error {
		err := b.hs.Close()
		<-b.done
		return err
	}(*a.bound)
}

func copiedLiteralParameterStart(a *copiedLiteralWrapper, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	launchCopiedServer(a.bound, ln)
	_, err := bind()
	return err
}

func launchReference(a *copiedReferenceAttempt, ln net.Listener) {
	go func() {
		defer close(a.done)
		if err := a.hs.Serve(ln); err != nil {
			_ = err
		}
	}()
}

func rollbackReferenceValue(a copiedReferenceAttempt) error {
	err := a.hs.Close()
	<-a.done
	return err
}

type referenceArgumentWrapper struct{ bound *copiedReferenceAttempt }

func (a *referenceArgumentWrapper) rollback() error { return rollbackReferenceValue(*a.bound) }

func referenceHelperArgumentStart(a *referenceArgumentWrapper, ln net.Listener) error {
	defer a.rollback()
	launchReference(a.bound, ln)
	_, err := bind()
	return err
}

type referenceLiteralWrapper struct{ bound *copiedReferenceAttempt }

func (a *referenceLiteralWrapper) rollback() error {
	return func(b copiedReferenceAttempt) error {
		err := b.hs.Close()
		<-b.done
		return err
	}(*a.bound)
}

func referenceLiteralParameterStart(a *referenceLiteralWrapper, ln net.Listener) error {
	defer a.rollback()
	launchReference(a.bound, ln)
	_, err := bind()
	return err
}

type mixedCopyAttempt struct {
	owned  http.Server
	shared *bracketBound
}

func (a *mixedCopyAttempt) rollback() error { return a.shared.rollback() }

func mutateCopiedReferences(a mixedCopyAttempt) { a.shared.hs = nil }

func copiedAggregateMutationStart(a *mixedCopyAttempt, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	launchBound(a.shared, ln)
	mutateCopiedReferences(*a)
	_, err := bind()
	return err
}

type referenceOwner struct {
	bound    *bracketBound
	metadata bool
}

func (a referenceOwner) clear() { a.bound.hs = nil }

func unknownReferenceAggregateStart(a *referenceOwner, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.bound.rollback()
	launchBound(a.bound, ln)
	referenceOwner.clear(*a)
	_, err := bind()
	return err
}

func touchReferenceMetadata(a referenceOwner) { a.metadata = true }

func boundedReferenceAggregateStart(a *referenceOwner, ln net.Listener) error {
	defer a.bound.rollback()
	launchBound(a.bound, ln)
	touchReferenceMetadata(*a)
	_, err := bind()
	return err
}

func (a mixedCopyAttempt) clearShared() { a.shared.hs = nil }

func storedCopiedReceiverStart(a *mixedCopyAttempt, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	launchBound(a.shared, ln)
	clear := a.clearShared
	clear()
	_, err := bind()
	return err
}

func consumeReferenceOwners(owners []referenceOwner) { owners[0].bound.hs = nil }

func nestedReferenceAggregateStart(a *referenceOwner, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.bound.rollback()
	launchBound(a.bound, ln)
	consumeReferenceOwners([]referenceOwner{*a})
	_, err := bind()
	return err
}

func storedSiblingMethodStart(a *siblingAttempt, ln net.Listener) error {
	defer a.two.rollback()
	launchBound(a.two, ln)
	clear := a.one.clearOther
	clear(a.one)
	_, err := bind()
	return err
}

type referenceHolder struct{ owner referenceOwner }

func transferReferenceOwner(a *referenceOwner) *referenceHolder {
	return &referenceHolder{owner: *a}
}

var transferredReferences *referenceHolder

func transferAfterFailureStart(a *referenceOwner, ln net.Listener) error {
	defer a.bound.rollback()
	launchBound(a.bound, ln)
	if _, err := bind(); err != nil {
		return err
	}
	transferredReferences = transferReferenceOwner(a)
	return nil
}

func transferBeforeFailureStart(a *referenceOwner, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.bound.rollback()
	launchBound(a.bound, ln)
	transferredReferences = transferReferenceOwner(a)
	if _, err := bind(); err != nil {
		return err
	}
	return nil
}

func conditionalTransferTailStart(a *referenceOwner, ln net.Listener, transfer bool) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.bound.rollback()
	launchBound(a.bound, ln)
	if _, err := bind(); err != nil {
		return err
	}
	if transfer {
		transferredReferences = transferReferenceOwner(a)
	}
	return nil
}

func nestedTransferTailStart(a *referenceOwner, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.bound.rollback()
	launchBound(a.bound, ln)
	if _, err := bind(); err != nil {
		return err
	}
	{
		transferredReferences = transferReferenceOwner(a)
	}
	return nil
}

func bareReturnTransferStart(a *referenceOwner, ln net.Listener) (err error) { // want `\[SLC100\].*publish serving before a later error return`
	defer a.bound.rollback()
	launchBound(a.bound, ln)
	transferredReferences = transferReferenceOwner(a)
	_, err = bind()
	return
}

func launchWithExternalReceiver(a *bracketBound, ln net.Listener) {
	_ = (http.Header{}).Get("unrelated")
	launchBound(a, ln)
}

func unrelatedExternalReceiverStart(a *siblingAttempt, ln net.Listener) error {
	defer a.one.rollback()
	launchWithExternalReceiver(a.one, ln)
	_, err := bind()
	return err
}

func externalAggregateArgumentStart(a *referenceOwner, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.bound.rollback()
	launchBound(a.bound, ln)
	_ = fmt.Sprint([]referenceOwner{*a})
	_, err := bind()
	return err
}

func externalAggregateReceiverStart(a *referenceOwner, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.bound.rollback()
	launchBound(a.bound, ln)
	_ = (&sync.Pool{New: func() any { return a.bound }}).Get()
	_, err := bind()
	return err
}
