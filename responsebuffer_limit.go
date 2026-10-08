package statute

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"statute.kjanat.dev/internal/parse"
)

const (
	defaultMaxResponseBodyBytes      int64 = 8 << 20
	defaultResponseBufferBudgetBytes int64 = 64 << 20
)

func resolveResponseBodyLimit(size string) (int64, error) {
	return resolveBufferSize(size, defaultMaxResponseBodyBytes)
}

func resolveBufferSize(size string, fallback int64) (int64, error) {
	if size == "" {
		return fallback, nil
	}
	n, err := parse.Size(size)
	if err != nil {
		return 0, err
	}
	if n <= 0 || uint64(n) > uint64(^uint(0)>>1) {
		return 0, fmt.Errorf("buffer size must be positive and fit in an int")
	}
	return n, nil
}

func resolveResponseBufferBudget(size string, limit int64) (int64, error) {
	n, err := resolveBufferSize(size, defaultResponseBufferBudgetBytes)
	if err != nil {
		return 0, err
	}
	if n < limit {
		return 0, fmt.Errorf("buffer budget must be at least the response body limit")
	}
	return n, nil
}

type responseBufferBudget struct {
	mu          sync.Mutex
	limit, used int64
}

func newResponseBufferBudget(limit int64) *responseBufferBudget {
	if limit <= 0 {
		limit = defaultResponseBufferBudgetBytes
	}
	return &responseBufferBudget{limit: limit}
}

func (b *responseBufferBudget) reserve(n int64) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > b.limit-b.used {
		return false
	}
	b.used += n
	return true
}

func (b *responseBufferBudget) release(n int64) {
	if b != nil {
		b.mu.Lock()
		b.used -= n
		b.mu.Unlock()
	}
}

var errResponseBufferLimit = errors.New("response buffer limit exceeded")

func newLimitedResponseBuffer(limit int64) *responseBuffer {
	b := newResponseBuffer()
	if limit <= 0 {
		limit = defaultMaxResponseBodyBytes
	}
	b.maxBodyBytes = limit
	return b
}

// ReverseProxy closes its upstream body after a copy error, then raises
// ErrAbortHandler. Convert that abort only when this buffer refused a write.
// Independent aborts and application panics retain their normal propagation.
func (b *responseBuffer) render(next http.Handler, r *http.Request) (complete bool) {
	ctx, cancel := context.WithCancel(r.Context())
	b.cancel = cancel
	defer cancel()
	defer func() {
		if value := recover(); value != nil {
			err, ok := value.(error)
			if !b.overLimit || !ok || !errors.Is(err, http.ErrAbortHandler) {
				panic(value)
			}
		}
	}()
	observeCacheCommit(next, b, r.WithContext(ctx), false)
	return !b.overLimit
}

func (b *responseBuffer) prepareBodyWrite(n int) bool {
	if b.overLimit || int64(n) > b.maxBodyBytes-int64(b.body.Len()) {
		b.overLimit = true
		if b.failureStatus == 0 {
			b.failureStatus = http.StatusBadGateway
		}
		b.cancelRender()
		return false
	}
	needed := b.body.Len() + n
	if needed <= b.body.Cap() {
		return true
	}
	oldCapacity := int64(b.body.Cap())
	grown := oldCapacity + min(oldCapacity, b.maxBodyBytes-oldCapacity)
	capacity := max(int64(needed), min(b.maxBodyBytes, max(512, grown)))
	// Both allocations are live while copying. Reserve the new allocation
	// before releasing the old one; slow replays retain their charge too.
	if !b.budget.reserve(capacity) {
		b.overLimit = true
		b.failureStatus = http.StatusServiceUnavailable
		b.cancelRender()
		return false
	}
	body := make([]byte, b.body.Len(), int(capacity))
	copy(body, b.body.Bytes())
	b.body = *bytes.NewBuffer(body)
	b.budget.release(oldCapacity)
	return true
}

func (b *responseBuffer) cancelRender() {
	if b.cancel != nil {
		b.cancel()
	}
}

func (b *responseBuffer) release() {
	capacity := b.body.Cap()
	b.body = bytes.Buffer{}
	b.budget.release(int64(capacity))
}

func writeResponseLimitFailure(w http.ResponseWriter, status int) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
}
