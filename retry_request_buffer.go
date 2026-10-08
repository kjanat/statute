package statute

import (
	"context"
	"io"
	"sync/atomic"
)

const initialRetryRequestBufferBytes = 512

// retryRequestBuffer tracks allocation ownership independently of each reader.
// Closing an attempt leaves bytes owned by other attempts or Timeout producers.
// body is mutable only during the initial synchronous read, before publication.
type retryRequestBuffer struct {
	budget *responseBufferBudget
	body   []byte
	refs   atomic.Int64
}

func newRetryRequestBuffer(budget *responseBufferBudget) *retryRequestBuffer {
	size := min(int64(initialRetryRequestBufferBytes), budget.limit)
	if !budget.reserve(size) {
		return nil
	}
	b := &retryRequestBuffer{budget: budget, body: make([]byte, 0, int(size))}
	b.refs.Store(1)
	return b
}

// readFrom never reads a byte without reserving its storage first. A false
// completion with no error requests a single attempt using body plus the
// unread original stream, whether the reason is growth pressure or oversize.
func (b *retryRequestBuffer) readFrom(r io.Reader) (complete bool, err error) {
	for {
		if len(b.body) == cap(b.body) && !b.grow() {
			return false, nil
		}
		n, err := r.Read(b.body[len(b.body):cap(b.body)])
		b.body = b.body[:len(b.body)+n]
		if err != nil && err != io.EOF {
			return false, err
		}
		if len(b.body) > maxRetryBufferBytes {
			return false, nil
		}
		if err == io.EOF {
			return true, nil
		}
	}
}

func (b *retryRequestBuffer) grow() bool {
	old := cap(b.body)
	size := min(old*2, maxRetryBufferBytes+1)
	if size <= old || !b.budget.reserve(int64(size)) {
		return false
	}
	body := make([]byte, len(b.body), size)
	copy(body, b.body)
	b.body = body
	b.budget.release(int64(old))
	return true
}

func (b *retryRequestBuffer) retain() { b.refs.Add(1) }

func (b *retryRequestBuffer) release() {
	if b.refs.Add(-1) != 0 {
		return
	}
	size := cap(b.body)
	b.body = nil
	b.budget.release(int64(size))
}

// Every nested Retry adds one immutable chain node. Timeout retains the entire
// inherited chain, including ancestors whose outer handler may already return.
type retryRequestBufferChain struct {
	buffer *retryRequestBuffer
	parent *retryRequestBufferChain
}

type retryRequestBuffersKey struct{}

func retryRequestBuffers(ctx context.Context) *retryRequestBufferChain {
	chain, _ := ctx.Value(retryRequestBuffersKey{}).(*retryRequestBufferChain)
	return chain
}

func withRetryRequestBuffer(ctx context.Context, b *retryRequestBuffer) context.Context {
	return context.WithValue(ctx, retryRequestBuffersKey{}, &retryRequestBufferChain{
		buffer: b, parent: retryRequestBuffers(ctx),
	})
}

func (c *retryRequestBufferChain) retain() {
	for node := c; node != nil; node = node.parent {
		node.buffer.retain()
	}
}

func (c *retryRequestBufferChain) release() {
	for node := c; node != nil; node = node.parent {
		node.buffer.release()
	}
}
