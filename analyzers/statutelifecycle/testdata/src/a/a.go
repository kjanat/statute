package a

import (
	"net"
	"net/http"
	"sync"
)

func bind() (net.Listener, error) { return nil, nil }
func sideEffect()                 {}

func callStart() error {
	sideEffect()
	return nil
}

func badStart(hs *http.Server) error { // want `\[SLC100\].*publish serving before a later error return`
	ln, err := bind()
	if err != nil {
		return err
	}
	go func() {
		if err := hs.Serve(ln); err != nil {
			_ = err
		}
	}()
	_, err = bind()
	if err != nil {
		return err
	}
	return nil
}

func goodStart(hs *http.Server) error {
	first, err := bind()
	if err != nil {
		return err
	}
	second, err := bind()
	if err != nil {
		return err
	}
	_ = second
	go func() {
		if err := hs.Serve(first); err != nil {
			_ = err
		}
	}()
	return nil
}

func ignoredServe(hs *http.Server, ln net.Listener) {
	_ = hs.Serve(ln) // want `\[SLC102\].*ignored Serve error`
}

type serverWrapper struct {
	hs *http.Server
}

func (w *serverWrapper) Serve(ln net.Listener) error {
	return w.hs.Serve(ln)
}

func ignoredWrapperServe(w *serverWrapper, ln net.Listener) {
	_ = w.Serve(ln) // want `\[SLC102\].*ignored Serve error`
}

type worker struct {
	stopCh chan struct{}
	done   chan struct{}
}

func (w *worker) start() {
	go func() {
		<-w.stopCh
		close(w.done)
	}()
}

func (w *worker) stop() {
	close(w.stopCh)
	<-w.done
}

func newWorker() *worker {
	return &worker{stopCh: make(chan struct{}), done: make(chan struct{})}
}

func newStartedWorker() *worker {
	w := newWorker()
	w.start() // want `\[SLC101\].*constructor newStartedWorker starts lifecycle-owned state`
	return w
}

type watcher struct {
	stopCh chan struct{}
	done   chan struct{}
}

func (w *watcher) start() { // want `\[SLC103\].*launches 2 lifecycle goroutine.*waits for only 1`
	go func() { <-w.stopCh }()
	go func() {
		<-w.stopCh
		close(w.done)
	}()
}

func (w *watcher) stop() {
	close(w.stopCh)
	<-w.done
}

type returnedWorker struct {
	stopCh chan struct{}
	done   chan struct{}
}

type returnedRun struct {
	worker *returnedWorker
}

type returnedRunAlias = *returnedRun

func (w *returnedWorker) start() (returnedRunAlias, error) { // want `\[SLC103\].*launches 2 lifecycle goroutine.*waits for only 1`
	r := &returnedRun{worker: w}
	go func() { <-w.stopCh }()
	go func() {
		<-w.stopCh
		close(w.done)
	}()
	return r, nil
}

func (r *returnedRun) stop() {
	close(r.worker.stopCh)
	<-r.worker.done
}

type runHolder struct {
	run *returnedRun
}

func newBuriedReturnedWorker(w *returnedWorker) *runHolder {
	run, _ := w.start() // want `\[SLC101\].*constructor newBuriedReturnedWorker starts lifecycle-owned state`
	return &runHolder{run: run}
}

func newReturningRun(w *returnedWorker) (*returnedRun, error) {
	return w.start() // want `\[SLC101\].*constructor newReturningRun starts lifecycle-owned state`
}

func wrapReturnedRun(run *returnedRun, _ error) *runHolder {
	return &runHolder{run: run}
}

func newWrappedReturnedWorker(w *returnedWorker) *runHolder {
	return wrapReturnedRun(w.start()) // want `\[SLC101\].*constructor newWrappedReturnedWorker starts lifecycle-owned state`
}

func discardReturnedOwner(w *returnedWorker) {
	w.start() // want `\[SLC101\].*discarded lifecycle owner returned by start`
}

func deferReturnedOwner(w *returnedWorker) {
	defer w.start() // want `\[SLC101\].*discarded lifecycle owner returned by start`
}

func launchReturnedOwner(w *returnedWorker) {
	go w.start() // want `\[SLC101\].*discarded lifecycle owner returned by start`
}

func discardAllReturnedResults(w *returnedWorker) {
	_, _ = w.start() // want `\[SLC101\].*discarded lifecycle owner returned by start`
}

func discardReturnedOwnerSlot(w *returnedWorker) {
	_, err := w.start() // want `\[SLC101\].*discarded lifecycle owner returned by start`
	_ = err
}

func retainReturnedOwner(w *returnedWorker) (*returnedRun, error) {
	run, err := w.start()
	return run, err
}

func returnReturnedOwner(w *returnedWorker) (*returnedRun, error) {
	return w.start()
}

func acceptReturnedOwner(*returnedRun, error) {}

func passReturnedOwner(w *returnedWorker) {
	acceptReturnedOwner(w.start())
}

type secondOwnerWorker struct{}
type secondOwnerRun struct{}

func (*secondOwnerWorker) start() (error, *secondOwnerRun) {
	return nil, &secondOwnerRun{}
}

func (*secondOwnerRun) stop() {}

func discardSecondOwnerSlot(w *secondOwnerWorker) {
	err, _ := w.start() // want `\[SLC101\].*discarded lifecycle owner returned by start`
	_ = err
}

func retainSecondOwner(w *secondOwnerWorker) (error, *secondOwnerRun) {
	return w.start()
}

type unrelatedStarter struct{}
type unrelatedResult struct{}

func (*unrelatedStarter) start() (*unrelatedResult, error) { return nil, nil }

func discardUnrelatedStart(u *unrelatedStarter) {
	u.start()
}

type stopRunWorker struct {
	stopCh chan struct{}
	done   chan struct{}
}
type stopRunOwner struct{ worker *stopRunWorker }

func (w *stopRunWorker) start() *stopRunOwner { // want `\[SLC103\].*launches 2 lifecycle goroutine.*stopRun.*waits for only 1`
	go func() { <-w.stopCh }()
	go func() {
		<-w.stopCh
		close(w.done)
	}()
	return &stopRunOwner{worker: w}
}

func (r *stopRunOwner) stopRun() {
	close(r.worker.stopCh)
	<-r.worker.done
}

type shutdownWorker struct {
	stopCh chan struct{}
	done   chan struct{}
}
type shutdownOwner struct{ worker *shutdownWorker }

func (w *shutdownWorker) start() *shutdownOwner { // want `\[SLC103\].*launches 2 lifecycle goroutine.*shutdown.*waits for only 1`
	go func() { <-w.stopCh }()
	go func() {
		<-w.stopCh
		close(w.done)
	}()
	return &shutdownOwner{worker: w}
}

func (r *shutdownOwner) shutdown() {
	close(r.worker.stopCh)
	<-r.worker.done
}

type waitGroupWorker struct{ stopCh chan struct{} }
type waitGroupRun struct {
	worker *waitGroupWorker
	wg     sync.WaitGroup
}

func (w *waitGroupWorker) start() *waitGroupRun { // want `\[SLC103\].*launches 2 lifecycle goroutine.*waits for only 0`
	r := &waitGroupRun{worker: w}
	r.wg.Go(func() { <-w.stopCh })
	r.wg.Go(func() { <-w.stopCh })
	return r
}

func (r *waitGroupRun) stop() { close(r.worker.stopCh) }

type joinedWaitGroupWorker struct{ stopCh chan struct{} }
type joinedWaitGroupRun struct {
	worker *joinedWaitGroupWorker
	wg     sync.WaitGroup
	once   sync.Once
}

func (w *joinedWaitGroupWorker) start() *joinedWaitGroupRun {
	r := &joinedWaitGroupRun{worker: w}
	r.wg.Go(func() { <-w.stopCh })
	r.wg.Go(func() { <-w.stopCh })
	return r
}

func (r *joinedWaitGroupRun) stop() {
	r.once.Do(func() {
		close(r.worker.stopCh)
		r.wg.Wait()
	})
}

type strongReceiverWorker struct{ wg sync.WaitGroup }
type weakReturnedRun struct{}

func (w *strongReceiverWorker) start() *weakReturnedRun { // want `\[SLC103\].*launches 2 lifecycle goroutine.*stop.*waits for only 0`
	w.wg.Go(func() {})
	w.wg.Go(func() {})
	return &weakReturnedRun{}
}

func (w *strongReceiverWorker) stop() { w.wg.Wait() }

func (*weakReturnedRun) stop() {}

type weakReceiverWorker struct{}
type strongReturnedRun struct{ wg sync.WaitGroup }

func (*weakReceiverWorker) start() *strongReturnedRun {
	r := &strongReturnedRun{}
	r.wg.Go(func() {})
	r.wg.Go(func() {})
	return r
}

func (*weakReceiverWorker) stop() {}

func (r *strongReturnedRun) stop() { r.wg.Wait() }

type onceWorker struct{ done chan struct{} }
type onceRun struct {
	worker *onceWorker
	once   sync.Once
}

func (w *onceWorker) start() *onceRun {
	go func() { close(w.done) }()
	return &onceRun{worker: w}
}

func (r *onceRun) stop() {
	r.once.Do(func() { <-r.worker.done })
}

type fakeGroup struct{}

func (*fakeGroup) Go(func()) {}

type fakeGroupWorker struct{ group fakeGroup }
type fakeGroupRun struct{}

func (w *fakeGroupWorker) start() *fakeGroupRun {
	w.group.Go(func() {})
	w.group.Go(func() {})
	return &fakeGroupRun{}
}

func (*fakeGroupRun) stop() {}

type callbackWorker struct{ done chan struct{} }
type waitGroupCallbackRun struct {
	worker *callbackWorker
	wg     sync.WaitGroup
}

func (w *callbackWorker) start() *waitGroupCallbackRun { // want `\[SLC103\].*launches 1 lifecycle goroutine.*waits for only 0`
	go func() { close(w.done) }()
	return &waitGroupCallbackRun{worker: w}
}

func (r *waitGroupCallbackRun) stop() {
	r.wg.Go(func() { <-r.worker.done })
}

type fakeOnce struct{}

func (*fakeOnce) Do(func()) {}

type fakeOnceWorker struct{ done chan struct{} }
type fakeOnceRun struct {
	worker *fakeOnceWorker
	once   fakeOnce
}

func (w *fakeOnceWorker) start() *fakeOnceRun { // want `\[SLC103\].*launches 1 lifecycle goroutine.*waits for only 0`
	go func() { close(w.done) }()
	return &fakeOnceRun{worker: w}
}

func (r *fakeOnceRun) stop() {
	r.once.Do(func() { <-r.worker.done })
}

type resource struct{}

func (*resource) Close() error { return nil }

type owner struct {
	r *resource
}

func (o *owner) Shutdown() error {
	_ = o.r.Close() // want `\[SLC104\].*cleanup discards Close error`
	return nil
}

func (o *owner) cleanShutdown() error {
	if err := o.r.Close(); err != nil {
		return err
	}
	return nil
}

type stopRunCleanupOwner struct{ r *resource }

func (o *stopRunCleanupOwner) stopRun() error {
	_ = o.r.Close() // want `\[SLC104\].*cleanup discards Close error`
	return nil
}

type localShutdownOwner struct{ r *resource }

func (o *localShutdownOwner) shutdown() error {
	_ = o.r.Close() // want `\[SLC104\].*cleanup discards Close error`
	return nil
}
