package statute

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"statute.kjanat.dev/resolved"
)

const defaultTimeoutMaxInFlight = 128

type timeoutHandler struct {
	next        http.Handler
	duration    time.Duration
	bodyLimit   int64
	budget      *responseBufferBudget
	maxInFlight int64
	active      atomic.Int64
}

func newTimeoutHandler(m resolved.Middleware, next http.Handler) *timeoutHandler {
	count := m.TimeoutMaxInFlight
	if count <= 0 {
		count = defaultTimeoutMaxInFlight
	}
	return &timeoutHandler{
		next: next, duration: m.Timeout, bodyLimit: m.MaxResponseBodyBytes,
		budget: newResponseBufferBudget(m.ResponseBufferBudgetBytes), maxInFlight: int64(count),
	}
}

func (h *timeoutHandler) acquire() bool {
	for {
		active := h.active.Load()
		if active >= h.maxInFlight {
			return false
		}
		if h.active.CompareAndSwap(active, active+1) {
			return true
		}
	}
}

func (h *timeoutHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.acquire() {
		writeResponseLimitFailure(w, http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.duration)
	defer cancel()
	buffer := newLimitedResponseBuffer(h.bodyLimit)
	buffer.budget, buffer.cancel = h.budget, cancel
	tw := &timeoutResponseWriter{buffer: buffer, ctx: ctx, done: make(chan struct{}), failed: make(chan struct{})}
	defer tw.finishCaller()
	leases := retryRequestBuffers(ctx)
	r = r.WithContext(retryTrailerContext(ctx, r.Trailer))
	leases.retain()
	go h.produce(tw, r, leases)
	select {
	case <-tw.done:
	case <-tw.failed:
	case <-ctx.Done():
	}
	tw.deliver(w)
}

func (h *timeoutHandler) produce(w *timeoutResponseWriter, r *http.Request, leases *retryRequestBufferChain) {
	returned := false
	defer func() {
		value := recover()
		if value == nil && !returned {
			value = http.ErrAbortHandler
		}
		w.finishProducer(value)
		leases.release()
		h.active.Add(-1)
		close(w.done)
	}()
	observeCacheCommit(h.next, w, r, true)
	returned = true
}

// Headers remain producer-owned until completion. Failure paths only read
// synchronized outcome state; they never inspect late producer header writes.
type timeoutResponseWriter struct {
	mu                       sync.Mutex
	buffer                   *responseBuffer
	ctx                      context.Context
	done, failed             chan struct{}
	err                      error
	panicValue               any
	producerDone, callerDone bool
}

func (w *timeoutResponseWriter) Header() http.Header { return w.buffer.header }

func (*timeoutResponseWriter) Push(string, *http.PushOptions) error { return http.ErrNotSupported }

func (w *timeoutResponseWriter) writeError() error {
	if w.err != nil {
		return w.err
	}
	if w.producerDone || w.callerDone {
		return http.ErrHandlerTimeout
	}
	if err := w.ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return http.ErrHandlerTimeout
		}
		return err
	}
	return nil
}

func (w *timeoutResponseWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.writeError(); err != nil {
		return 0, err
	}
	n, err := w.buffer.Write(p)
	if err != nil {
		w.err = err
		close(w.failed)
	}
	return n, err
}

func (w *timeoutResponseWriter) WriteHeader(status int) {
	if status < 100 || status > 999 {
		panic(fmt.Sprintf("invalid WriteHeader code %d", status))
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.writeError() == nil {
		w.buffer.WriteHeader(status)
	}
}

func (w *timeoutResponseWriter) finishProducer(value any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err, ok := value.(error); w.buffer.overLimit && ok && errors.Is(err, http.ErrAbortHandler) {
		value = nil
	}
	w.panicValue, w.producerDone = value, true
	if w.callerDone {
		w.buffer.release()
	}
}

func (w *timeoutResponseWriter) finishCaller() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.callerDone = true
	if w.producerDone {
		w.buffer.release()
	}
}

type timeoutResult struct {
	buffer     *responseBuffer
	status     int
	deadline   bool
	panicValue any
}

func (w *timeoutResponseWriter) result() timeoutResult {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.producerDone && w.panicValue != nil {
		return timeoutResult{panicValue: w.panicValue}
	}
	if w.buffer.overLimit {
		return timeoutResult{status: w.buffer.failureStatus}
	}
	w.err = w.ctx.Err()
	if w.err == nil && w.producerDone {
		return timeoutResult{buffer: w.buffer}
	}
	deadline := errors.Is(w.err, context.DeadlineExceeded)
	if deadline {
		w.err = http.ErrHandlerTimeout
	}
	return timeoutResult{deadline: deadline}
}

func (w *timeoutResponseWriter) deliver(dst http.ResponseWriter) {
	result := w.result()
	switch {
	case result.panicValue != nil:
		panic(result.panicValue)
	case result.status != 0:
		writeResponseLimitFailure(dst, result.status)
	case result.buffer != nil:
		result.buffer.replay(dst)
	default:
		dst.WriteHeader(http.StatusServiceUnavailable)
		if result.deadline {
			_, _ = io.WriteString(dst, "request timed out")
		}
	}
}
