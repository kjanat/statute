package statute

import (
	"io"
	"net/http"
	"sync"
)

func retryRequestLeaseTransport(base http.RoundTripper) http.RoundTripper {
	return &retryRequestTransport{base: base}
}

type retryRequestTransport struct{ base http.RoundTripper }

func (t *retryRequestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	chain := retryRequestBuffers(r.Context())
	if chain == nil || r.Body == nil || r.Body == http.NoBody {
		return t.base.RoundTrip(r)
	}
	// Rewinds can close the previous reader before calling GetBody. Keep an
	// enclosing owner until RoundTrip returns so replacements retain live leases.
	chain.retain()
	defer chain.release()
	owner := &retryTransportOwner{chain: chain}
	defer func() {
		if value := recover(); value != nil {
			owner.closeAll()
			panic(value)
		}
	}()
	forwarded := r.WithContext(r.Context())
	forwarded.Body = owner.wrap(r.Body)
	if getBody := r.GetBody; getBody != nil {
		forwarded.GetBody = func() (io.ReadCloser, error) {
			body, err := getBody()
			if err == nil && body != nil && body != http.NoBody {
				body = owner.wrap(body)
			}
			return body, err
		}
	}
	return t.base.RoundTrip(forwarded)
}

// Only open wrappers occupy this intrusive registry. Completed rewinds leave
// no historical entries; panic cleanup takes each wrapper out before closing.
type retryTransportOwner struct {
	mu    sync.Mutex
	chain *retryRequestBufferChain
	head  *retryTransportBody
}

func (o *retryTransportOwner) wrap(body io.ReadCloser) *retryTransportBody {
	o.chain.retain()
	b := &retryTransportBody{reader: body, owner: o, linked: true}
	o.mu.Lock()
	b.next = o.head
	if o.head != nil {
		o.head.prev = b
	}
	o.head = b
	o.mu.Unlock()
	return b
}

func (o *retryTransportOwner) remove(b *retryTransportBody) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.removeLocked(b)
}

func (o *retryTransportOwner) removeLocked(b *retryTransportBody) {
	if !b.linked {
		return
	}
	if b.prev == nil {
		o.head = b.next
	} else {
		b.prev.next = b.next
	}
	if b.next != nil {
		b.next.prev = b.prev
	}
	b.prev, b.next, b.linked = nil, nil, false
}

func (o *retryTransportOwner) closeAll() {
	for {
		o.mu.Lock()
		b := o.head
		if b != nil {
			o.removeLocked(b)
		}
		o.mu.Unlock()
		if b == nil {
			return
		}
		closeRetryTransportAfterPanic(b)
	}
}

func closeRetryTransportAfterPanic(b *retryTransportBody) {
	// Cleanup must preserve the transport's original panic, including when
	// a custom reader also panics while closing.
	defer func() { _ = recover() }()
	_ = b.Close()
}

type retryTransportBody struct {
	mu                sync.Mutex
	reader            io.ReadCloser
	owner             *retryTransportOwner
	active            int
	closed, closeDone bool
	closeErr          error
	// Registry links are protected only by owner.mu.
	prev, next *retryTransportBody
	linked     bool
}

func (b *retryTransportBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return 0, http.ErrBodyReadAfterClose
	}
	b.active++
	reader := b.reader
	b.mu.Unlock()
	defer b.finishRead()
	return reader.Read(p)
}

func (b *retryTransportBody) Close() (err error) {
	b.mu.Lock()
	if b.closed {
		err = b.closeErr
		b.mu.Unlock()
		return err
	}
	b.closed = true
	reader := b.reader
	b.mu.Unlock()
	b.owner.remove(b)
	defer func() { b.finishClose(err) }()
	return reader.Close()
}

func (b *retryTransportBody) finishRead() {
	b.mu.Lock()
	b.active--
	chain := b.retireLocked()
	b.mu.Unlock()
	chain.release()
}

func (b *retryTransportBody) finishClose(err error) {
	b.mu.Lock()
	b.closeDone, b.closeErr = true, err
	chain := b.retireLocked()
	b.mu.Unlock()
	chain.release()
}

func (b *retryTransportBody) retireLocked() *retryRequestBufferChain {
	if !b.closeDone || b.active != 0 || b.reader == nil {
		return nil
	}
	b.reader = nil
	return b.owner.chain
}
