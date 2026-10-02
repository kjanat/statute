package htmlrewrite

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type failurePolicy uint8

const (
	failClosed failurePolicy = iota + 1
	failOpen
)

// httpEngine owns the shared code and admission budget; policy stays on routes.
type httpEngine struct {
	engine *engine
	mu     sync.Mutex
	active map[*rewrittenBody]struct{}
	limit  int
	closed bool
}

func newHTTPEngine(ctx context.Context, concurrency int) (*httpEngine, error) {
	if concurrency < 1 {
		return nil, errors.New("positive rewrite concurrency required")
	}
	e, err := newEngine(ctx)
	if err != nil {
		return nil, err
	}
	return &httpEngine{engine: e, active: make(map[*rewrittenBody]struct{}), limit: concurrency}, nil
}

func (e *httpEngine) close() error {
	e.mu.Lock()
	e.closed = true
	bodies := make([]*rewrittenBody, 0, len(e.active))
	for b := range e.active {
		bodies = append(bodies, b)
	}
	e.mu.Unlock()
	// Cancel all readers before joining any one of them.
	for _, b := range bodies {
		b.cancel()
	}
	var err error
	for _, b := range bodies {
		err = errors.Join(err, b.Close())
	}
	return errors.Join(err, e.engine.close())
}

type httpPolicy struct {
	failure     failurePolicy
	inputLimit  int64
	outputLimit int
	timeout     time.Duration
}

// A transport wrapper is created per route; base can be shared unchanged.
type rewriteTransport struct {
	owner     *httpEngine
	base      http.RoundTripper
	policy    httpPolicy
	rewritten atomic.Int64
	bypassed  atomic.Int64
	rejected  atomic.Int64
	failed    atomic.Int64
}

func newRewriteTransport(e *httpEngine, base http.RoundTripper, p httpPolicy) (*rewriteTransport, error) {
	if e == nil || base == nil || (p.failure != failOpen && p.failure != failClosed) || p.inputLimit < 1 || p.outputLimit < 1 || p.timeout <= 0 {
		return nil, errors.New("explicit failure policy and positive rewrite limits required")
	}
	return &rewriteTransport{owner: e, base: base, policy: p}, nil
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet || req.Header.Get("Upgrade") != "" {
		return t.base.RoundTrip(req)
	}
	// The timer also bounds upstream reads. It lives until response-body close.
	ctx, cancel := context.WithTimeout(req.Context(), t.policy.timeout)
	clone := req.Clone(ctx)
	for _, name := range []string{"If-None-Match", "If-Modified-Since", "Range", "If-Range"} {
		clone.Header.Del(name)
	}
	res, err := t.base.RoundTrip(clone)
	if err != nil {
		cancel()
		return nil, err
	}
	res.Body = &cancelBody{ReadCloser: res.Body, cancel: cancel}
	if res.StatusCode != http.StatusOK {
		return res, nil
	}
	if len(res.Header.Values("Content-Type")) > 1 {
		return t.reject(res, errors.New("ambiguous response content type"))
	}
	media, params, mediaErr := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if mediaErr == nil && media != "text/html" {
		return res, nil
	}
	if res.Header.Get("Content-Type") == "" {
		return res, nil
	}
	var unsupported error
	switch {
	case mediaErr != nil:
		unsupported = errors.New("invalid response content type")
	case params["charset"] != "" && !strings.EqualFold(params["charset"], "utf-8") && !strings.EqualFold(params["charset"], "us-ascii"):
		unsupported = errors.New("unsupported HTML charset")
	case strings.Join(res.Header.Values("Content-Encoding"), ",") != "" && !strings.EqualFold(strings.Join(res.Header.Values("Content-Encoding"), ","), "identity"):
		unsupported = errors.New("unsupported HTML content encoding")
	case hasNoTransform(res.Header):
		unsupported = errors.New("HTML response forbids transformation")
	case res.Header.Get("Content-Range") != "" || len(res.Trailer) != 0 || res.Header.Get("Trailer") != "":
		unsupported = errors.New("HTML ranges and trailers are unsupported")
	case res.ContentLength > t.policy.inputLimit:
		unsupported = errors.New("HTML input limit exceeded")
	}
	if unsupported != nil {
		return t.reject(res, unsupported)
	}

	t.owner.mu.Lock()
	if t.owner.closed || len(t.owner.active) >= t.owner.limit {
		t.owner.mu.Unlock()
		return t.reject(res, errors.New("HTML rewrite capacity unavailable"))
	}
	b := &rewrittenBody{source: res.Body, cancel: cancel, ctx: ctx, owner: t.owner, remaining: t.policy.inputLimit, failed: &t.failed, response: res}
	b.stream, err = t.owner.engine.newStream(ctx, &b.output, t.policy.outputLimit)
	if err == nil {
		t.owner.active[b] = struct{}{}
	}
	t.owner.mu.Unlock()
	if err != nil {
		return t.reject(res, fmt.Errorf("HTML rewrite initialization: %w", err))
	}
	res.Body = b
	t.rewritten.Add(1)
	res.ContentLength = -1
	res.Header = res.Header.Clone()
	// Origin validators describe the original representation.
	for _, name := range []string{"Content-Length", "Content-Encoding", "ETag", "Last-Modified", "Content-MD5", "Digest", "Content-Digest", "Repr-Digest", "Accept-Ranges"} {
		res.Header.Del(name)
	}
	return res, nil
}

func (t *rewriteTransport) reject(res *http.Response, err error) (*http.Response, error) {
	if t.policy.failure == failOpen {
		t.bypassed.Add(1)
		res.Header = res.Header.Clone()
		res.Header.Add("Cache-Control", "no-store")
		return res, nil
	}
	t.rejected.Add(1)
	_ = res.Body.Close()
	return nil, err
}

func hasNoTransform(h http.Header) bool {
	for _, value := range h.Values("Cache-Control") {
		for _, directive := range strings.Split(value, ",") {
			name, _, _ := strings.Cut(strings.TrimSpace(directive), "=")
			if strings.EqualFold(name, "no-transform") {
				return true
			}
		}
	}
	return false
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
	err    error
}

func (b *cancelBody) Close() error {
	b.once.Do(func() { b.cancel(); b.err = b.ReadCloser.Close() })
	return b.err
}

// Rewriting is pull-driven: no producer goroutine can outrun the downstream.
// Close cancels/unblocks the source before joining an outstanding Read.
type rewrittenBody struct {
	mu        sync.Mutex
	source    io.ReadCloser
	stream    *stream
	output    bytes.Buffer
	input     [4096]byte
	remaining int64
	ctx       context.Context
	cancel    context.CancelFunc
	owner     *httpEngine
	terminal  error
	closed    bool
	failed    *atomic.Int64
	response  *http.Response
}

func (b *rewrittenBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	if b.closed {
		return 0, b.terminal
	}
	if err := b.ctx.Err(); err != nil {
		b.fail(err)
	}
	for b.output.Len() == 0 && b.terminal == nil {
		if err := b.ctx.Err(); err != nil {
			b.fail(err)
			break
		}
		n, err := b.source.Read(b.input[:])
		if int64(n) > b.remaining {
			b.fail(errors.New("HTML input limit exceeded"))
			break
		}
		b.remaining -= int64(n)
		if n > 0 {
			if writeErr := b.stream.write(b.input[:n]); writeErr != nil {
				b.fail(writeErr)
				break
			}
		}
		if err == io.EOF {
			if len(b.response.Trailer) != 0 {
				b.fail(errors.New("unexpected HTML response trailers"))
			} else if finishErr := b.stream.finish(); finishErr != nil {
				b.fail(finishErr)
			} else {
				b.terminal = io.EOF
			}
		} else if err != nil {
			b.fail(err)
		}
		if n == 0 && err == nil {
			return 0, nil
		}
	}
	if b.output.Len() > 0 {
		return b.output.Read(p)
	}
	err := b.terminal
	b.closeLocked()
	return 0, err
}

func (b *rewrittenBody) fail(err error) {
	if b.terminal == nil || b.terminal == io.EOF {
		b.failed.Add(1)
	}
	b.output.Reset()
	b.terminal = err
}

func (b *rewrittenBody) closeLocked() {
	if b.closed {
		return
	}
	b.closed = true
	if b.terminal == nil {
		b.terminal = io.ErrClosedPipe
	}
	b.cancel()
	_ = b.source.Close()
	_ = b.stream.close()
	b.output = bytes.Buffer{}
	b.owner.mu.Lock()
	delete(b.owner.active, b)
	b.owner.mu.Unlock()
}

func (b *rewrittenBody) Close() error {
	b.cancel()
	err := b.source.Close()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closeLocked()
	return err
}
