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
	// Tests replace removeSpool to simulate file removal errors.
	removeSpool func(string) error
}

func newRequestBufferManager(cfg resolved.RequestBufferResources) *requestBufferManager {
	if cfg.MaxInFlight == 0 {
		cfg.MaxInFlight = requestBufferMaxInFlight
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &requestBufferManager{cfg: cfg, ctx: ctx, cancel: cancel, removeSpool: os.Remove}
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
		mu       sync.Mutex
		deadline func(time.Time) error
		reading  int
	}
)

// Every route and middleware request copy uses the same server manager.
func (m *requestBufferManager) wrap(next http.Handler) http.Handler {
	return preserveRequestTrailers(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := &requestBufferRequest{manager: m, deadline: http.NewResponseController(w).SetReadDeadline}
		defer state.finish()
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestBufferContextKey{}, state)))
	}))
}

// Synchronize reads and deadline calls with the outer HTTP handler returning.
// Its response writer can only be used while that handler is running.
func (s *requestBufferRequest) beginRead() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deadline == nil {
		return false
	}
	s.reading++
	return true
}

func (s *requestBufferRequest) endRead() {
	s.mu.Lock()
	s.reading--
	s.mu.Unlock()
}

func (s *requestBufferRequest) interruptRead() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deadline != nil {
		_ = s.deadline(time.Now())
	}
}

func (s *requestBufferRequest) finish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reading != 0 && s.deadline != nil {
		_ = s.deadline(time.Now())
	}
	s.deadline = nil
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

// Each GetBody reader keeps the buffer reserved until Close and any active Read
// finish. Once the buffer is released, GetBody returns an error.
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

// Remove the temporary filename immediately. The open file stays readable until
// the last reader finishes, including after cancellation and shutdown. A failed
// create or remove operation rejects the request.
func (b *requestBodyBuffer) spill(maximum int64) error {
	if !b.manager.reserveDisk(maximum) {
		return errors.New("request buffer disk budget exhausted")
	}
	b.disk = maximum
	file, err := os.CreateTemp(b.manager.cfg.Storage, "statute-request-*")
	if err != nil {
		return err
	}
	if err := b.manager.removeSpool(file.Name()); err != nil {
		_ = file.Close()
		_ = b.manager.removeSpool(file.Name())
		return err
	}
	b.file = file
	_, err = file.Write(b.data[:int(b.size)])
	return err
}

func requestBufferHandler(policy resolved.Middleware, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state, _ := r.Context().Value(requestBufferContextKey{}).(*requestBufferRequest)
		if r.ContentLength > policy.RequestBufferMaxBytes {
			rejectRequestBuffer(w, r, state, http.StatusRequestEntityTooLarge)
			return
		}
		if state == nil {
			rejectRequestBuffer(w, r, state, http.StatusServiceUnavailable)
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
			rejectRequestBuffer(w, r, state, http.StatusServiceUnavailable)
			return
		}
		defer b.release()
		if ctx.Err() != nil {
			rejectRequestBuffer(w, r, state, http.StatusServiceUnavailable)
			return
		}
		// Read through the original request so trailers, including unannounced
		// HTTP/3 trailers, are available on that request after EOF.
		status := b.consume(ctx, state, r, policy.RequestBufferMaxBytes, limit)
		if status == 0 && ctx.Err() != nil {
			status = http.StatusServiceUnavailable
		}
		if status != 0 {
			rejectRequestBuffer(w, r, state, status)
			return
		}
		next.ServeHTTP(w, b.forward(r, ctx))
	})
}

func rejectRequestBuffer(w http.ResponseWriter, r *http.Request, state *requestBufferRequest, status int) {
	// HTTP/1 drains unread body bytes before sending a response. Stop that read
	// before rejecting an unfinished upload.
	if r.ProtoMajor == 1 && r.Body != nil && r.Body != http.NoBody {
		if state != nil {
			state.interruptRead()
		} else {
			_ = http.NewResponseController(w).SetReadDeadline(time.Now())
		}
	}
	writeResponseLimitFailure(w, status)
}

func (b *requestBodyBuffer) forward(r *http.Request, ctx context.Context) *http.Request {
	forwarded := r.WithContext(withRequestBodyLease(snapshotRetryTrailerNames(ctx, r.Trailer), b))
	forwarded.Body, forwarded.GetBody = b.reader(), b.getBody
	forwarded.Trailer = r.Trailer.Clone()
	forwarded.ContentLength = b.size
	forwarded.TransferEncoding = nil
	if len(forwarded.Trailer) != 0 {
		forwarded.Body = &retryTrailerReadCloser{ReadCloser: forwarded.Body, trailers: forwarded.Trailer}
	} else if b.size == 0 {
		forwarded.Body = http.NoBody
	}
	return forwarded
}

func (b *requestBodyBuffer) consume(ctx context.Context, state *requestBufferRequest, r *http.Request, maximum, threshold int64) int {
	if r.Body == nil || r.Body == http.NoBody {
		return 0
	}
	if !state.beginRead() {
		return http.StatusServiceUnavailable
	}
	defer state.endRead()
	// Close may wait for unread HTTP/1 bytes. Stop an incomplete upload before
	// closing its body, and keep cancellation active until Close finishes.
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { state.interruptRead(); close(done) })
	complete := false
	defer func() {
		defer func() {
			if !stop() {
				<-done
			}
		}()
		if !complete {
			state.interruptRead()
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
			complete = done
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
