package htmlrewrite

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"
)

const programABI uint32 = 1

type eventKind uint32

const (
	elementEvent eventKind = iota + 1
	textEvent
	commentEvent
	endTagEvent
	documentEndEvent
)

type operation uint32

const (
	setAttribute operation = iota + 1
	removeAttribute
	beforeContent
	afterContent
	replaceContent
	removeContent
	prependContent
	appendContent
	innerContent
)

type command struct {
	op      operation
	html    bool // Explicit trusted HTML; false always means escaped text.
	name    string
	content string
}

type programRule struct {
	selector string
	event    eventKind
	commands []command
	callback bool
}

type programLimits struct {
	programBytes, eventBytes, commandBytes, commands, scopes, events int
	inputBytes, outputBytes, parserBytes                             int
}

type programDefinition struct {
	rules      []programRule
	newSession func(context.Context, requestMetadata) (callbackSession, error)
}

type requestMetadata struct {
	method, originalMethod, url string
	headers                     map[string][]string
}

func (m requestMetadata) copy() requestMetadata {
	copy := m
	copy.headers = make(map[string][]string, len(m.headers))
	for name, values := range m.headers {
		copy.headers[name] = append([]string(nil), values...)
	}
	return copy
}

type callbackSession interface {
	handle(context.Context, eventSnapshot, *mutations) error
	close() error
}

type invocationIdentity struct {
	stream, invocation, token, element uint64
	rule                               uint32
	kind                               eventKind
}

type attributeSnapshot struct{ name, entityValue string }

// Attribute values and text include entity references.
type eventSnapshot struct {
	id                  invocationIdentity
	textUnit            uint64
	chunk               uint32
	last, canContent    bool
	removed             bool
	name, text, context string
	attributes          []attributeSnapshot
	scopes              []uint64
}

type mutations struct {
	mu       sync.Mutex
	active   bool
	event    eventSnapshot
	limits   programLimits
	commands []command
	bytes    int
	err      error
}

func validateCommand(kind eventKind, canContent bool, c command) error {
	if !utf8.ValidString(c.name) || !utf8.ValidString(c.content) || strings.ContainsRune(c.name, 0) {
		return errors.New("mutation contains invalid UTF-8 or attribute name")
	}
	valid := false
	switch c.op {
	case setAttribute, removeAttribute:
		valid = kind == elementEvent && c.name != "" && !c.html && !strings.ContainsAny(c.name, " \n\r\t\f/>=")
	case beforeContent, afterContent, removeContent:
		valid = kind >= elementEvent && kind <= endTagEvent
	case replaceContent:
		valid = kind >= elementEvent && kind <= commentEvent
	case prependContent, innerContent:
		valid = kind == elementEvent && canContent
	case appendContent:
		valid = kind == documentEndEvent || kind == elementEvent && canContent
	}
	if c.op != setAttribute && c.op != removeAttribute && c.name != "" {
		valid = false
	}
	if (c.op == removeAttribute || c.op == removeContent) && (c.content != "" || c.html) {
		valid = false
	}
	if !valid {
		return fmt.Errorf("operation %d is invalid for event %d", c.op, kind)
	}
	return nil
}

type (
	invocationContextKey struct{}
	invocationScope      struct {
		id   invocationIdentity
		fail func(error)
	}
)

func (m *mutations) add(ctx context.Context, c command) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var scope invocationScope
	var ok bool
	if ctx != nil {
		scope, ok = ctx.Value(invocationContextKey{}).(invocationScope)
	}
	if !m.active {
		err := errors.New("mutation invocation has ended")
		if ok {
			scope.fail(err)
		}
		return err
	}
	if !ok || scope.id != m.event.id {
		err := errors.New("mutation belongs to another invocation")
		if ok {
			scope.fail(err)
		} else {
			m.err = err
		}
		return err
	}
	if m.err != nil {
		return m.err
	}
	err := validateCommand(m.event.id.kind, m.event.canContent, c)
	size := 16 + len(c.name) + len(c.content)
	if err == nil && (len(m.commands) >= m.limits.commands || size > m.limits.commandBytes-m.bytes) {
		err = errors.New("mutation command budget exceeded")
	}
	if err != nil {
		m.err = err // Ignoring an invalid mutation cannot silently drop policy.
		return err
	}
	m.bytes += size
	m.commands = append(m.commands, c)
	return nil
}

func (m *mutations) finish() ([]command, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.active = false
	return append([]command(nil), m.commands...), m.err
}

type wireWriter struct{ bytes []byte }

func (w *wireWriter) u32(n uint32) { w.bytes = binary.LittleEndian.AppendUint32(w.bytes, n) }
func (w *wireWriter) u64(n uint64) { w.bytes = binary.LittleEndian.AppendUint64(w.bytes, n) }
func (w *wireWriter) str(s string) {
	w.u32(uint32(len(s)))
	w.bytes = append(w.bytes, s...)
}

func (w *wireWriter) identity(id invocationIdentity) {
	w.u32(programABI)
	w.u64(id.stream)
	w.u64(id.invocation)
	w.u64(id.token)
	w.u64(id.element)
	w.u32(id.rule)
	w.u32(uint32(id.kind))
}

func (w *wireWriter) command(c command) {
	w.u32(uint32(c.op))
	var html uint32
	if c.html {
		html = 1
	}
	w.u32(html)
	w.str(c.name)
	w.str(c.content)
}

type wireReader struct {
	bytes []byte
	err   error
}

func (r *wireReader) take(n uint32) []byte {
	if r.err != nil || uint64(n) > uint64(len(r.bytes)) {
		r.err = errors.New("truncated ABI message")
		return nil
	}
	result := r.bytes[:n]
	r.bytes = r.bytes[n:]
	return result
}

func (r *wireReader) u32() uint32 {
	b := r.take(4)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint32(b)
}

func (r *wireReader) u64() uint64 {
	b := r.take(8)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint64(b)
}

func (r *wireReader) str() string {
	b := r.take(r.u32())
	if !utf8.Valid(b) {
		r.err = errors.New("invalid UTF-8 in ABI message")
	}
	return string(b)
}

func (r *wireReader) flag() bool {
	n := r.u32()
	if n > 1 {
		r.err = errors.New("invalid ABI boolean")
	}
	return n == 1
}

func encodeProgram(p programDefinition, limits programLimits, streamID uint64) ([]byte, error) {
	for _, n := range []int{limits.programBytes, limits.eventBytes, limits.commandBytes, limits.commands, limits.scopes, limits.events, limits.inputBytes, limits.outputBytes, limits.parserBytes} {
		if n <= 0 || uint64(n) > uint64(^uint32(0)) {
			return nil, errors.New("program limits must be positive uint32 values")
		}
	}
	if limits.programBytes < 40 || limits.programBytes > 65536 || limits.commandBytes < 48 || limits.eventBytes < 80 || streamID == 0 || len(p.rules) > 1024 {
		return nil, errors.New("invalid program protocol limits")
	}
	w := wireWriter{}
	w.u32(programABI)
	w.u64(streamID)
	for _, n := range []int{limits.eventBytes, limits.commandBytes, limits.commands, limits.scopes, limits.parserBytes, limits.events} {
		w.u32(uint32(n))
	}
	w.u32(uint32(len(p.rules)))
	for _, rule := range p.rules {
		if rule.event < elementEvent || rule.event > documentEndEvent || !utf8.ValidString(rule.selector) || rule.event != documentEndEvent && rule.selector == "" || rule.event == documentEndEvent && rule.selector != "" || rule.callback && (p.newSession == nil || len(rule.commands) != 0) {
			return nil, errors.New("invalid program rule or missing session factory")
		}
		if len(rule.selector) > limits.programBytes-len(w.bytes)-16 {
			return nil, errors.New("program byte limit exceeded")
		}
		w.str(rule.selector)
		w.u32(uint32(rule.event))
		var callback uint32
		if rule.callback {
			callback = 1
		}
		w.u32(callback)
		if len(rule.commands) > limits.commands {
			return nil, errors.New("program command count exceeded")
		}
		w.u32(uint32(len(rule.commands)))
		commandBytes := 48
		for _, c := range rule.commands {
			if err := validateCommand(rule.event, true, c); err != nil {
				return nil, err
			}
			if len(c.name) > limits.programBytes-len(w.bytes)-16 || len(c.content) > limits.programBytes-len(w.bytes)-16-len(c.name) {
				return nil, errors.New("program byte limit exceeded")
			}
			commandBytes += 16 + len(c.name) + len(c.content)
			if commandBytes > limits.commandBytes {
				return nil, errors.New("program command byte limit exceeded")
			}
			w.command(c)
		}
		if len(w.bytes) > limits.programBytes {
			return nil, errors.New("program byte limit exceeded")
		}
	}
	return w.bytes, nil
}
