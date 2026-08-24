package a

import (
	"net"
	"net/http"
)

func bind() (net.Listener, error) { return nil, nil }

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
