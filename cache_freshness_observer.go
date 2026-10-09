package statute

import (
	"context"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type cacheObservationKey struct{}

// Late Timeout producers retain only standalone request admission state.
type cacheObservation struct {
	mu     sync.Mutex
	first  time.Time
	unsafe atomic.Bool
}

func cacheObservationFrom(ctx context.Context) *cacheObservation {
	o, _ := ctx.Value(cacheObservationKey{}).(*cacheObservation)
	return o
}

func (o *cacheObservation) receipt(now time.Time) time.Time {
	if o == nil {
		return now
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.first.IsZero() && o.first.Before(now) {
		return o.first
	}
	return now
}

func (o *cacheObservation) record(now time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.first.IsZero() || now.Before(o.first) {
		o.first = now
	}
}

func (o *cacheObservation) usable() bool { return o == nil || !o.unsafe.Load() }

type cacheCommitObserver struct {
	http.ResponseWriter
	observation *cacheObservation
	committed   time.Time
	snapshot    cacheFreshness
	multiplexed bool
}

func (w *cacheCommitObserver) capture() {
	if !w.committed.IsZero() {
		return
	}
	w.committed = time.Now()
	w.observation.record(w.committed)
	w.snapshot = w.policy()
	if !w.snapshot.valid {
		w.observation.unsafe.Store(true)
	}
}

func (w *cacheCommitObserver) policy() cacheFreshness {
	return cacheObservedPolicy(w.Header(), w.committed)
}

func cacheObservedPolicy(h http.Header, committed time.Time) cacheFreshness {
	if _, ok := cacheHeaderCost(h); !ok || !cacheResponseAllowsStorage(h) {
		return cacheFreshness{}
	}
	return cacheResponseFreshness(h, committed, committed, time.Duration(math.MaxInt64))
}

func (w *cacheCommitObserver) finish() {
	w.capture()
	if w.snapshot != w.policy() {
		w.observation.unsafe.Store(true)
	}
}

func (w *cacheCommitObserver) WriteHeader(status int) {
	if status == http.StatusSwitchingProtocols && w.multiplexed {
		return
	}
	if status >= 200 || status == http.StatusSwitchingProtocols {
		w.capture()
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *cacheCommitObserver) Write(p []byte) (int, error) {
	w.capture()
	return w.ResponseWriter.Write(p)
}

func (w *cacheCommitObserver) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type cacheBufferObserver struct{ *cacheCommitObserver }

func (w cacheBufferObserver) Flush() {}

type cacheTimeoutObserver struct{ *cacheCommitObserver }

func (w cacheTimeoutObserver) Push(target string, options *http.PushOptions) error {
	return w.ResponseWriter.(http.Pusher).Push(target, options)
}

func observeCacheCommit(next http.Handler, w http.ResponseWriter, r *http.Request, timeout bool) {
	o := cacheObservationFrom(r.Context())
	if o == nil {
		next.ServeHTTP(w, r)
		return
	}
	observer := &cacheCommitObserver{ResponseWriter: w, observation: o, multiplexed: r.ProtoMajor >= 2}
	if timeout {
		next.ServeHTTP(cacheTimeoutObserver{observer}, r)
	} else {
		next.ServeHTTP(cacheBufferObserver{observer}, r)
	}
	observer.finish()
}
