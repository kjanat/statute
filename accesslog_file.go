package statute

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
)

// Each successful Start owns one destination and queue. The sink itself is
// reusable after failed startup; handlers never retain a retired file handle.
type accessLogSink struct {
	path     string
	size     int
	fallback io.Writer
	current  atomic.Pointer[accessLogRun]
}
type accessLogRun struct {
	sink      *accessLogSink
	writer    io.Writer
	file      *os.File
	queue     chan []byte
	done      chan struct{}
	stopping  chan struct{}
	abort     chan struct{}
	mu        sync.RWMutex
	closed    bool
	once      sync.Once
	abortOnce sync.Once
	fileOnce  sync.Once
	errMu     sync.Mutex
	err       error
}

func (s *accessLogSink) start() (*accessLogRun, error) {
	run := &accessLogRun{sink: s, writer: s.fallback, stopping: make(chan struct{}), abort: make(chan struct{})}
	if s.path != "" {
		file, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, err
		}
		run.file, run.writer = file, file
	}
	if run.writer == nil {
		return nil, errors.New("access log has no destination")
	}
	if s.size > 0 {
		run.queue = make(chan []byte, s.size)
		run.done = make(chan struct{})
		go func() {
			defer close(run.done)
			for data := range run.queue {
				select {
				case <-run.abort:
					return
				default:
				}
				_, err := run.writer.Write(data)
				run.recordError(err)
			}
		}()
	}
	s.current.Store(run)
	return run, nil
}

func (s *accessLogSink) Write(data []byte) (int, error) {
	run := s.current.Load()
	if run == nil {
		return 0, io.ErrClosedPipe
	}
	run.mu.RLock()
	if run.closed {
		run.mu.RUnlock()
		return 0, io.ErrClosedPipe
	}
	if run.queue != nil {
		defer run.mu.RUnlock()
		select {
		case run.queue <- append([]byte(nil), data...):
			return len(data), nil
		case <-run.stopping:
			return 0, io.ErrClosedPipe
		}
	}
	run.mu.RUnlock()
	return run.writer.Write(data)
}

func (r *accessLogRun) recordError(err error) {
	if err == nil {
		return
	}
	r.errMu.Lock()
	if r.err == nil {
		r.err = err
	}
	r.errMu.Unlock()
}

func (r *accessLogRun) stop(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.once.Do(func() {
		r.sink.current.CompareAndSwap(r, nil)
		close(r.stopping)
		r.mu.Lock()
		r.closed = true
		if r.queue != nil {
			close(r.queue)
		}
		r.mu.Unlock()
	})
	if r.done != nil {
		select {
		case <-r.done:
		case <-ctx.Done():
			r.abortOnce.Do(func() { close(r.abort) })
			r.recordError(ctx.Err())
		}
	}
	if r.file != nil {
		r.fileOnce.Do(func() { r.recordError(r.file.Close()) })
	}
	r.errMu.Lock()
	defer r.errMu.Unlock()
	return r.err
}
