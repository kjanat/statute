package a

import (
	"net"
	"net/http"
)

type bracketAttempt struct {
	hs   *http.Server
	ln   net.Listener
	done chan struct{}
}

func (a *bracketAttempt) rollback() error {
	err := a.hs.Close()
	<-a.done
	return err
}

func (a *bracketAttempt) serveEarly() {
	go func() {
		defer close(a.done)
		if err := a.hs.Serve(a.ln); err != nil {
			_ = err
		}
	}()
}

func bracketStart(a *bracketAttempt) error {
	defer a.rollback()
	a.serveEarly()
	if _, err := bind(); err != nil {
		return err
	}
	return nil
}

func bracketFuncLitDeferStart(a *bracketAttempt) error {
	defer func() { _ = a.rollback() }()
	a.serveEarly()
	if _, err := bind(); err != nil {
		return err
	}
	return nil
}

type bracketBound struct {
	hs   *http.Server
	done chan struct{}
}

func (b *bracketBound) rollback() error {
	err := b.hs.Close()
	<-b.done
	return err
}

type bracketNestedAttempt struct {
	bound *bracketBound
}

func (a *bracketNestedAttempt) rollback() error {
	return a.bound.rollback()
}

func (a *bracketNestedAttempt) serveEarly(ln net.Listener) {
	go func() {
		defer close(a.bound.done)
		if err := a.bound.hs.Serve(ln); err != nil {
			_ = err
		}
	}()
}

func bracketTransitiveStopStart(a *bracketNestedAttempt, ln net.Listener) error {
	defer func() { _ = a.rollback() }()
	a.serveEarly(ln)
	if _, err := bind(); err != nil {
		return err
	}
	return nil
}

func earlyServeNoDeferStart(a *bracketAttempt) error { // want `\[SLC100\].*publish serving before a later error return`
	a.serveEarly()
	if _, err := bind(); err != nil {
		return err
	}
	return nil
}

func earlyServeDeferAfterPublishStart(a *bracketAttempt) error { // want `\[SLC100\].*publish serving before a later error return`
	a.serveEarly()
	defer a.rollback()
	if _, err := bind(); err != nil {
		return err
	}
	return nil
}

func earlyServeWrongRootStart(x, y *bracketAttempt) error { // want `\[SLC100\].*publish serving before a later error return`
	defer x.rollback()
	y.serveEarly()
	if _, err := bind(); err != nil {
		return err
	}
	return nil
}

type bracketServer struct{}

func (s *bracketServer) serveHealthEarly(a *bracketAttempt) {
	a.serveEarly()
}

func earlyServeHelperRootStart(s *bracketServer, a *bracketAttempt) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	s.serveHealthEarly(a)
	if _, err := bind(); err != nil {
		return err
	}
	return nil
}

type listenerOnlyAttempt struct {
	hs   *http.Server
	ln   net.Listener
	done chan struct{}
}

func (a *listenerOnlyAttempt) rollback() error {
	err := a.ln.Close()
	<-a.done
	return err
}

func (a *listenerOnlyAttempt) serveEarly() {
	go func() {
		defer close(a.done)
		if err := a.hs.Serve(a.ln); err != nil {
			_ = err
		}
	}()
}

func earlyServeListenerOnlyRollbackStart(a *listenerOnlyAttempt) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	a.serveEarly()
	if _, err := bind(); err != nil {
		return err
	}
	return nil
}

type noWaitAttempt struct {
	hs *http.Server
	ln net.Listener
}

func (a *noWaitAttempt) rollback() error {
	return a.hs.Close()
}

func (a *noWaitAttempt) serveEarly() {
	go func() {
		if err := a.hs.Serve(a.ln); err != nil {
			_ = err
		}
	}()
}

func earlyServeNoWaitStart(a *noWaitAttempt) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	a.serveEarly()
	if _, err := bind(); err != nil {
		return err
	}
	return nil
}

func earlyServeSecondUnownedPublisherStart(a *bracketAttempt, hs *http.Server, ln net.Listener) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	a.serveEarly()
	go func() {
		if err := hs.Serve(ln); err != nil {
			_ = err
		}
	}()
	if _, err := bind(); err != nil {
		return err
	}
	return nil
}

type crossOwnerWaiter struct {
	done chan struct{}
}

type crossOwnerAttempt struct {
	hs    *http.Server
	ln    net.Listener
	extra *crossOwnerWaiter
}

func (a *crossOwnerAttempt) rollback() error {
	err := a.hs.Close()
	<-a.extra.done
	return err
}

func (a *crossOwnerAttempt) serveEarly() {
	go func() {
		if err := a.hs.Serve(a.ln); err != nil {
			_ = err
		}
	}()
}

func earlyServeCrossOwnerWaitStart(a *crossOwnerAttempt) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	a.serveEarly()
	if _, err := bind(); err != nil {
		return err
	}
	return nil
}

type unwaitedBound struct {
	hs   *http.Server
	done chan struct{}
}

func (b *unwaitedBound) rollback() error {
	return b.hs.Close()
}

type waiterHelper struct {
	done chan struct{}
}

func (w *waiterHelper) wait() {
	<-w.done
}

type uncorrelatedAttempt struct {
	bound  *unwaitedBound
	helper *waiterHelper
	ln     net.Listener
}

func (a *uncorrelatedAttempt) rollback() error {
	err := a.bound.rollback()
	a.helper.wait()
	return err
}

func (a *uncorrelatedAttempt) serveEarly() {
	go func() {
		defer close(a.bound.done)
		if err := a.bound.hs.Serve(a.ln); err != nil {
			_ = err
		}
	}()
}

func earlyServeUncorrelatedWaitStart(a *uncorrelatedAttempt) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	a.serveEarly()
	if _, err := bind(); err != nil {
		return err
	}
	return nil
}

type extraServer struct {
	hs *http.Server
	ln net.Listener
}

func (o *extraServer) serve() {
	go func() {
		if err := o.hs.Serve(o.ln); err != nil {
			_ = err
		}
	}()
}

type twoServerAttempt struct {
	health   *bracketBound
	extra    *extraServer
	healthLn net.Listener
}

func (a *twoServerAttempt) rollback() error {
	return a.health.rollback()
}

func (a *twoServerAttempt) serveHealth() {
	go func() {
		defer close(a.health.done)
		if err := a.health.hs.Serve(a.healthLn); err != nil {
			_ = err
		}
	}()
}

func (a *twoServerAttempt) serveExtra() {
	a.extra.serve()
}

func earlyServeUnstoppedSecondOwnerStart(a *twoServerAttempt) error { // want `\[SLC100\].*publish serving before a later error return`
	defer a.rollback()
	a.serveHealth()
	a.serveExtra()
	if _, err := bind(); err != nil {
		return err
	}
	return nil
}
