package statute

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"statute.kjanat.dev/resolved"
)

const requestBufferMaxInFlight = 128

type requestBufferManager struct {
	cfg          resolved.RequestBufferResources
	mu           sync.Mutex
	memory, disk int64
	active       int
	closed       bool
	ctx          context.Context
	cancel       context.CancelFunc
}

func newRequestBufferManager(cfg resolved.RequestBufferResources) *requestBufferManager {
	if cfg.MaxInFlight == 0 {
		cfg.MaxInFlight = requestBufferMaxInFlight
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &requestBufferManager{cfg: cfg, ctx: ctx, cancel: cancel}
}

func (m *requestBufferManager) stopAdmission() { m.mu.Lock(); m.closed = true; m.mu.Unlock() }

func (m *requestBufferManager) acquire(size int64) *requestBodyBuffer {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.active >= m.cfg.MaxInFlight || size > m.cfg.MemoryBudgetBytes-m.memory {
		return nil
	}
	m.memory += size
	m.active++
	return &requestBodyBuffer{manager: m, refs: 1, memory: size, data: make([]byte, int(size))}
}

func (m *requestBufferManager) reserveDisk(size int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if size > m.cfg.DiskBudgetBytes-m.disk {
		return false
	}
	m.disk += size
	return true
}

type (
	requestBufferContextKey struct{}
	requestBufferRequest    struct {
		manager  *requestBufferManager
		deadline func(time.Time) error
	}
)

// The context carries server resource ownership through every routing tier and
// middleware clone. Compiled middleware and Docker generations own no budgets.
func (m *requestBufferManager) wrap(next http.Handler) http.Handler {
	return preserveRequestTrailers(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := &requestBufferRequest{manager: m, deadline: http.NewResponseController(w).SetReadDeadline}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestBufferContextKey{}, state)))
	}))
}

type requestBodyBuffer struct {
	manager      *requestBufferManager
	mu           sync.Mutex
	refs         int
	memory, disk int64
	data         []byte
	file         *os.File
	size         int64
}

func (b *requestBodyBuffer) retain() { b.mu.Lock(); b.refs++; b.mu.Unlock() }
func (b *requestBodyBuffer) release() {
	b.mu.Lock()
	b.refs--
	last := b.refs == 0
	b.mu.Unlock()
	if !last {
		return
	}
	if b.file != nil {
		_ = b.file.Close()
	}
	b.data = nil
	b.manager.mu.Lock()
	b.manager.memory -= b.memory
	b.manager.disk -= b.disk
	b.manager.active--
	b.manager.mu.Unlock()
}

func (b *requestBodyBuffer) reader() io.ReadCloser {
	if b.file != nil {
		return io.NopCloser(io.NewSectionReader(b.file, 0, b.size))
	}
	return io.NopCloser(bytes.NewReader(b.data[:int(b.size)]))
}

// A GetBody reader has its own lease, including Read/Close overlap. The factory
// cannot resurrect retired storage after the enclosing request has completed.
func (b *requestBodyBuffer) getBody() (io.ReadCloser, error) {
	b.mu.Lock()
	if b.refs == 0 {
		b.mu.Unlock()
		return nil, http.ErrBodyReadAfterClose
	}
	b.refs++
	b.mu.Unlock()
	defer b.release()
	owner := &retryTransportOwner{chain: &retryRequestBufferChain{lease: b}}
	return owner.wrap(b.reader()), nil
}

// Spool files are unlinked before publication. Their open descriptor owns the
// bytes until the last lease ends, including after cancellation and shutdown.
// Failure to create or unlink is an admission failure, never streaming fallback.
func (b *requestBodyBuffer) spill(maximum int64) error {
	if !b.manager.reserveDisk(maximum) {
		return errors.New("disk admission unavailable")
	}
	b.disk = maximum
	file, err := os.CreateTemp(b.manager.cfg.Storage, "statute-request-*")
	if err != nil {
		return err
	}
	if err := os.Remove(file.Name()); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return err
	}
	b.file = file
	_, err = file.Write(b.data[:int(b.size)])
	return err
}

func requestBufferHandler(policy resolved.Middleware, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > policy.RequestBufferMaxBytes {
			writeResponseLimitFailure(w, http.StatusRequestEntityTooLarge)
			return
		}
		state, _ := r.Context().Value(requestBufferContextKey{}).(*requestBufferRequest)
		if state == nil {
			writeResponseLimitFailure(w, http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		stopServer := state.manager.onShutdown(cancel)
		defer stopServer()
		limit := policy.RequestBufferMaxBytes
		if state.manager.cfg.Storage != "" {
			limit = min(limit, policy.RequestBufferMemoryThresholdBytes)
		}
		b := state.manager.acquire(limit + 1)
		if b == nil {
			writeResponseLimitFailure(w, http.StatusServiceUnavailable)
			return
		}
		defer b.release()
		if ctx.Err() != nil {
			writeResponseLimitFailure(w, http.StatusServiceUnavailable)
			return
		}
		// Capture transport trailers only after EOF, including unannounced HTTP/3
		// trailers. Keep the original request and its body associated during reading.
		status := b.consume(ctx, state, r, policy.RequestBufferMaxBytes, limit)
		if status == 0 && ctx.Err() != nil {
			status = http.StatusServiceUnavailable
		}
		if status != 0 {
			writeResponseLimitFailure(w, status)
			return
		}
		forwarded := r.WithContext(withRequestBodyLease(snapshotRetryTrailerNames(ctx, r.Trailer), b))
		forwarded.Body, forwarded.GetBody = b.reader(), b.getBody
		forwarded.Trailer = r.Trailer.Clone()
		forwarded.ContentLength = b.size
		forwarded.TransferEncoding = nil
		if len(forwarded.Trailer) != 0 {
			forwarded.Body = &retryTrailerReadCloser{ReadCloser: forwarded.Body, trailers: forwarded.Trailer}
		}
		next.ServeHTTP(w, forwarded)
	})
}

func (b *requestBodyBuffer) consume(ctx context.Context, state *requestBufferRequest, r *http.Request, maximum, threshold int64) int {
	if r.Body == nil || r.Body == http.NoBody {
		return 0
	}
	// Deadline interruption does not close the live reader concurrently with EOF
	// trailer publication. Join the callback before leaving the buffering phase.
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = state.deadline(time.Now()); close(done) })
	defer func() {
		if !stop() {
			<-done
		}
		_ = r.Body.Close()
	}()
	emptyReads := 0
	for {
		if ctx.Err() != nil {
			return http.StatusServiceUnavailable
		}
		done, status := b.readChunk(ctx, r.Body, maximum, threshold, &emptyReads)
		if done || status != 0 {
			return status
		}
	}
}

func (b *requestBodyBuffer) readChunk(ctx context.Context, reader io.Reader, maximum, threshold int64, emptyReads *int) (bool, int) {
	target := b.data
	if b.file == nil {
		target = target[int(b.size):]
	}
	target = target[:min(int64(len(target)), maximum-b.size+1)]
	n, err := reader.Read(target)
	if n == 0 && err == nil {
		*emptyReads++
	} else {
		*emptyReads = 0
	}
	if *emptyReads >= 100 {
		return false, http.StatusBadRequest
	}
	b.size += int64(n)
	if b.size > maximum {
		return false, http.StatusRequestEntityTooLarge
	}
	if status := requestBufferReadStatus(ctx, err); status != 0 {
		return false, status
	}
	if status := b.storeChunk(target[:n], maximum, threshold); status != 0 {
		return false, status
	}
	return errors.Is(err, io.EOF), 0
}

func (m *requestBufferManager) onShutdown(cancel context.CancelFunc) func() bool {
	return context.AfterFunc(m.ctx, cancel)
}

func requestBufferReadStatus(ctx context.Context, err error) int {
	if err == nil || errors.Is(err, io.EOF) {
		return 0
	}
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return http.StatusRequestEntityTooLarge
	}
	if ctx.Err() != nil {
		return http.StatusServiceUnavailable
	}
	return http.StatusBadRequest
}

func (b *requestBodyBuffer) storeChunk(data []byte, maximum, threshold int64) int {
	if b.file != nil {
		if _, err := b.file.Write(data); err != nil {
			return http.StatusServiceUnavailable
		}
	} else if b.size > threshold {
		if err := b.spill(maximum); err != nil {
			return http.StatusServiceUnavailable
		}
	}
	return 0
}
