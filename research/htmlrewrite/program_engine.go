//go:build statute_htmlrewrite

package htmlrewrite

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tetratelabs/wazero/api"
)

type preparedProgram struct {
	definition programDefinition
	limits     programLimits
	timeout    time.Duration
	wire       []byte
}

func prepareProgram(p programDefinition, l programLimits, timeout time.Duration) (*preparedProgram, error) {
	if timeout <= 0 || l.eventBytes > 1<<20 || l.commandBytes > 1<<20 || l.scopes > 65536 {
		return nil, errors.New("invalid program timeout or allocation limit")
	}
	copy := p
	copy.rules = append([]programRule(nil), p.rules...)
	for i := range copy.rules {
		copy.rules[i].commands = append([]command(nil), p.rules[i].commands...)
	}
	wire, err := encodeProgram(copy, l, 1)
	if err != nil {
		return nil, err
	}
	return &preparedProgram{copy, l, timeout, wire}, nil
}

type programRun struct {
	engine             *engine
	mu                 sync.Mutex
	max                int
	next               uint64
	children           map[uint64]*programChild
	sealed, finalizing bool
	done               chan struct{}
	cleanup            error
	finalized          atomic.Int32
}
type programChild struct {
	run               *programRun
	id                uint64
	ctx               context.Context
	cancel            context.CancelFunc
	cancelOnce        sync.Once
	program           *preparedProgram
	session           callbackSession
	invocation, token uint64
	mu                sync.Mutex
	err               error
	capture           bool
	replies           [][]byte
	events            [][]byte
}
type programContextKey struct{}

func newProgramRun(ctx context.Context, capacity int) (*programRun, error) {
	if capacity <= 0 {
		return nil, errors.New("admission must be positive")
	}
	e, err := newEngine(ctx)
	if err != nil {
		return nil, err
	}
	return &programRun{engine: e, max: capacity, children: make(map[uint64]*programChild), done: make(chan struct{})}, nil
}

func (r *programRun) admit(ctx context.Context, p *preparedProgram) (*programChild, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sealed || len(r.children) >= r.max || r.next == ^uint64(0) {
		return nil, errors.New("program admission unavailable")
	}
	r.next++
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	c := &programChild{run: r, id: r.next, ctx: ctx, cancel: cancel, program: p}
	r.children[c.id] = c
	return c, nil
}

func applicationCall(fn func() error) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("application callback panicked")
		}
	}()
	return fn()
}

func (r *programRun) start(ctx context.Context, p *preparedProgram, metadata requestMetadata, w io.Writer) (_ *programStream, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p == nil || w == nil {
		return nil, errors.New("missing program or writer")
	}
	c, err := r.admit(ctx, p)
	if err != nil {
		return nil, err
	}
	accepted := false
	defer func() {
		if !accepted {
			cleanup := c.closeSession()
			c.cancelSession()
			r.release(c.id, cleanup)
			err = errors.Join(err, cleanup)
		}
	}()
	if factory := p.definition.newSession; factory != nil {
		err = applicationCall(func() error {
			var e error
			c.session, e = factory(c.ctx, metadata.copy())
			if e != nil {
				c.session = nil
				return e
			}
			if c.session == nil {
				return errors.New("factory returned nil session")
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if err = c.ctx.Err(); err != nil {
		return nil, err
	}
	wire := append([]byte(nil), p.wire...)
	for i := range 8 {
		wire[4+i] = byte(c.id >> (8 * i))
	}
	s, err := r.engine.newConfiguredStream(context.WithValue(c.ctx, programContextKey{}, c), w, p.limits.outputBytes, true, wire)
	if err != nil {
		return nil, err
	}
	accepted = true
	return &programStream{stream: s, child: c}, nil
}

func (c *programChild) closeSession() error {
	if c.session == nil {
		return nil
	}
	session := c.session
	c.session = nil
	return applicationCall(session.close)
}

func (r *programRun) release(id uint64, err error) {
	r.mu.Lock()
	delete(r.children, id)
	r.cleanup = errors.Join(r.cleanup, err)
	finalize := r.sealed && len(r.children) == 0 && !r.finalizing
	if finalize {
		r.finalizing = true
	}
	r.mu.Unlock()
	if finalize {
		err := r.engine.close()
		r.mu.Lock()
		r.cleanup = errors.Join(r.cleanup, err)
		r.finalized.Add(1)
		close(r.done)
		r.mu.Unlock()
	}
}

func (r *programRun) shutdown(ctx context.Context) error {
	r.mu.Lock()
	r.sealed = true
	r.mu.Unlock()
	r.release(0, nil)
	select {
	case <-r.done:
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.cleanup
	default:
	}
	select {
	case <-r.done:
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.cleanup
	case <-ctx.Done():
		r.mu.Lock()
		children := make([]*programChild, 0, len(r.children))
		for _, c := range r.children {
			children = append(children, c)
		}
		r.mu.Unlock()
		for _, c := range children {
			c.cancelSession()
		}
		return fmt.Errorf("rewrite shutdown has unfinished work: %w", ctx.Err())
	}
}
func (r *programRun) active() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.children) }

type programStream struct {
	*stream
	child      *programChild
	mu         sync.Mutex
	closed     bool
	inputBytes int
	terminal   error
}

func (s *programStream) enter() error {
	if !s.mu.TryLock() {
		err := errors.New("concurrent or reentrant program call")
		s.child.fail(err)
		return err
	}
	if s.closed {
		s.mu.Unlock()
		return errors.New("program stream has ended")
	}
	return nil
}

func (s *programStream) retire(err error) error {
	if !s.closed {
		s.closed = true
		cleanup := s.child.closeSession()
		moduleErr := s.stream.close()
		s.child.cancelSession()
		s.child.run.release(s.child.id, errors.Join(cleanup, moduleErr))
		s.terminal = errors.Join(err, cleanup, moduleErr)
	}
	return s.terminal
}

func (s *programStream) write(b []byte) error {
	if err := s.enter(); err != nil {
		return err
	}
	defer s.mu.Unlock()
	if err := errors.Join(s.child.failure(), s.child.ctx.Err()); err != nil {
		return s.retire(err)
	}
	if len(b) > s.child.program.limits.inputBytes-s.inputBytes {
		return s.retire(errors.New("program input budget exceeded"))
	}
	s.inputBytes += len(b)
	if err := errors.Join(s.stream.write(b), s.child.failure()); err != nil {
		return s.retire(err)
	}
	return nil
}

func (s *programStream) finish() error {
	if err := s.enter(); err != nil {
		return err
	}
	defer s.mu.Unlock()
	if err := errors.Join(s.child.failure(), s.child.ctx.Err()); err != nil {
		return s.retire(err)
	}
	return s.retire(errors.Join(s.stream.invoke("finish"), s.child.failure()))
}

func (s *programStream) close() error {
	if !s.mu.TryLock() {
		err := errors.New("program stream is still in use")
		s.child.fail(err)
		return err
	}
	defer s.mu.Unlock()
	return s.retire(nil)
}

func (c *programChild) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = errors.Join(c.err, err)
}
func (c *programChild) failure() error { c.mu.Lock(); defer c.mu.Unlock(); return c.err }
func decodeEvent(bytes []byte, c *programChild) (eventSnapshot, error) {
	r := wireReader{bytes: bytes}
	version := r.u32()
	event := eventSnapshot{}
	event.id = invocationIdentity{stream: r.u64(), invocation: r.u64(), token: r.u64(), element: r.u64(), rule: r.u32(), kind: eventKind(r.u32())}
	event.textUnit = r.u64()
	event.chunk = r.u32()
	event.last = r.flag()
	event.canContent = r.flag()
	event.removed = r.flag()
	event.name = r.str()
	event.text = r.str()
	event.context = r.str()
	count := r.u32()
	if uint64(count) > uint64(len(r.bytes)/8) {
		return event, errors.New("invalid attribute count")
	}
	for range count {
		event.attributes = append(event.attributes, attributeSnapshot{r.str(), r.str()})
	}
	scopes := r.u32()
	if uint64(scopes) > uint64(len(r.bytes)/8) || uint64(scopes) > uint64(c.program.limits.scopes) {
		return event, errors.New("invalid scope count")
	}
	for range scopes {
		event.scopes = append(event.scopes, r.u64())
	}
	if r.err != nil {
		return event, r.err
	}
	if len(r.bytes) != 0 || version != programABI || event.id.stream != c.id || event.id.invocation != c.invocation+1 || event.id.token == 0 || event.id.token < c.token || uint64(event.id.rule) >= uint64(len(c.program.definition.rules)) {
		return event, errors.New("invalid invocation identity")
	}
	rule := c.program.definition.rules[event.id.rule]
	if rule.event != event.id.kind || !rule.callback || c.session == nil {
		return event, errors.New("invalid callback event binding")
	}
	c.invocation = event.id.invocation
	c.token = event.id.token
	return event, nil
}

// Copy the event and write the reply while Rust owns the token borrow.
func programExchange(ctx context.Context, module api.Module, stack []uint64) {
	c, ok := ctx.Value(programContextKey{}).(*programChild)
	if !ok {
		stack[0] = 0
		return
	}
	pointer, n, replyPointer, capacity := api.DecodeU32(stack[0]), api.DecodeU32(stack[1]), api.DecodeU32(stack[2]), api.DecodeU32(stack[3])
	stack[0] = 0
	bytes, ok := module.Memory().Read(pointer, n)
	if !ok || uint64(n) > uint64(c.program.limits.eventBytes) {
		c.fail(errors.New("invalid event memory or budget"))
		return
	}
	event, err := decodeEvent(bytes, c)
	if err != nil {
		c.fail(err)
		return
	}
	if c.capture {
		c.events = append(c.events, append([]byte(nil), bytes...))
	}
	m := &mutations{active: true, event: event, limits: c.program.limits, bytes: 48}
	invocationCtx := context.WithValue(c.ctx, invocationContextKey{}, invocationScope{id: event.id, fail: c.fail})
	err = applicationCall(func() error { return c.session.handle(invocationCtx, event, m) })
	commands, mutationErr := m.finish()
	c.fail(errors.Join(err, mutationErr, c.ctx.Err()))
	if c.failure() != nil {
		return
	}
	w := wireWriter{}
	w.identity(event.id)
	w.u32(uint32(len(commands)))
	for _, command := range commands {
		w.command(command)
	}
	if uint64(len(w.bytes)) > uint64(capacity) || len(w.bytes) > c.program.limits.commandBytes || !module.Memory().Write(replyPointer, w.bytes) {
		c.fail(errors.New("invalid reply memory or budget"))
		return
	}
	if c.capture {
		c.replies = append(c.replies, append([]byte(nil), w.bytes...))
	}
	stack[0] = uint64(len(w.bytes))
}

func (c *programChild) cancelSession() { c.cancelOnce.Do(c.cancel) }
