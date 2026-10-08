package statute

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// cacheWriter never exposes an escape hatch around the budget. Once streaming,
// Header still returns the original producer map so retained maps and trailers
// remain usable. No cache failure cancels the producer or synthesizes a status.
type cacheWriter struct {
	outer     http.ResponseWriter
	cache     *ttlCache
	entry     *cacheEntry
	buf       *responseBuffer
	streaming bool
	flushed   bool
	noStore   bool
	err       error
	ctx       context.Context
	requested time.Time
	received  time.Time
	freshness cacheFreshness
	snapshot  cacheFreshness
}

func newCacheWriter(w http.ResponseWriter, c *ttlCache, e *cacheEntry) *cacheWriter {
	b := newLimitedResponseBuffer(c.maxBody)
	b.budget = c.budget
	c.attach(e, b)
	return &cacheWriter{outer: w, cache: c, entry: e, buf: b, ctx: context.Background(), requested: time.Now()}
}

func (w *cacheWriter) Header() http.Header { return w.buf.header }

func (w *cacheWriter) WriteHeader(code int) {
	if w.streaming || (code < 200 && code != http.StatusSwitchingProtocols) {
		return
	}
	w.captureFreshness()
	w.buf.WriteHeader(code)
	if _, ok := cacheHeaderCost(w.buf.header); !ok {
		_ = w.startStreaming()
	}
}

func (w *cacheWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if !w.streaming {
		w.captureFreshness()
		if _, ok := cacheHeaderCost(w.buf.header); ok {
			if n, err := w.buf.Write(p); err == nil {
				return n, nil
			}
		}
		if err := w.startStreaming(); err != nil {
			return 0, err
		}
	}
	n, err := cacheWrite(w.outer, p)
	if err == nil && !w.flushed {
		err = w.flushStreaming()
	}
	if err != nil {
		w.err = err
	}
	return n, err
}

func (w *cacheWriter) startStreaming() error {
	if w.streaming {
		return w.err
	}
	w.checkObservedPolicy()
	w.streaming = true
	cacheCommitHeaders(w.outer, w.buf.header, w.buf.status)
	if w.buf.body.Len() != 0 {
		_, w.err = cacheWrite(w.outer, w.buf.body.Bytes())
	}
	// The candidate keeps its entry slot until ServeHTTP exits, but delivered
	// body storage is released immediately. Deferred lease cleanup covers panic.
	w.buf.release()
	return w.err
}

// Flush after the triggering bytes reach the writer: HTTP content sniffing
// needs both the buffered prefix and the first overflow write. Streaming
// framing then preserves trailers introduced after that write returns.
func (w *cacheWriter) flushStreaming() error {
	w.flushed = true
	err := http.NewResponseController(w.outer).Flush()
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

func (w *cacheWriter) Flush() { _ = w.FlushError() }

func (w *cacheWriter) FlushError() error {
	if !w.streaming {
		return nil
	}
	if w.err != nil {
		return w.err
	}
	// An unsupported flush is not a failed body write: callers can still
	// continue delivering through a writer which has no streaming interface.
	return http.NewResponseController(w.outer).Flush()
}

// The fixed copy buffer fits the scratch reservation. Hiding ReaderFrom on the
// destination forces every byte through Write's admission and streaming path.
func (w *cacheWriter) ReadFrom(r io.Reader) (int64, error) {
	var scratch [32 << 10]byte
	n, err := io.CopyBuffer(struct{ io.Writer }{w}, r, scratch[:])
	if err != nil {
		w.noStore = true
	}
	return n, err
}

func (w *cacheWriter) finish(ctx context.Context, key cacheKey, request http.Header, requestAllowsStorage bool, policy *cacheProxyPolicy) {
	w.captureFreshness()
	w.checkObservedPolicy()
	if w.streaming {
		cacheFinishTrailers(w.outer, w.buf.header)
		return
	}
	if _, ok := cacheHeaderCost(w.buf.header); !ok {
		_ = w.startStreaming()
		cacheFinishTrailers(w.outer, w.buf.header)
		return
	}
	names, storable := cacheStorageVary(ctx, w.buf, requestAllowsStorage, policy)
	w.freshness = w.freshness.constrain(w.currentFreshness())
	w.entry.freshness = w.freshness
	buf := *w.buf
	if w.freshness.valid {
		buf.header = w.buf.header.Clone()
		w.freshness.apply(buf.header, time.Now())
	}
	// Keep the body charged through delivery, and publish only after successful
	// writes. A failed downstream delivery must not introduce a new entry.
	if err := cacheReplay(w.outer, &buf); err == nil && storable && !w.noStore && cachePolicyUsable(policy) && cacheObservationFrom(ctx).usable() {
		w.cache.publish(w.entry, key, request, names, &buf, policy)
	}
}

func (w *cacheWriter) checkObservedPolicy() {
	if w.snapshot != cacheObservedPolicy(w.buf.header, w.received) {
		if observation := cacheObservationFrom(w.ctx); observation != nil {
			observation.unsafe.Store(true)
		}
	}
}

func (w *cacheWriter) captureFreshness() {
	if !w.received.IsZero() {
		return
	}
	w.received = cacheObservationFrom(w.ctx).receipt(time.Now())
	if observation := cacheObservationFrom(w.ctx); observation != nil {
		observation.record(w.received)
	}
	w.snapshot = cacheObservedPolicy(w.buf.header, w.received)
	if !w.snapshot.valid {
		if observation := cacheObservationFrom(w.ctx); observation != nil {
			observation.unsafe.Store(true)
		}
	}
	w.freshness = w.currentFreshness()
}

func (w *cacheWriter) currentFreshness() cacheFreshness {
	if !cacheObservationFrom(w.ctx).usable() || !cacheProjectionPreflight(w.ctx, w.buf.header) || !cacheResponseAllowsStorage(w.buf.header) {
		return cacheFreshness{}
	}
	projected := responseHeadersForCache(w.ctx, w.buf.header)
	if !cacheResponseAllowsStorage(projected) {
		return cacheFreshness{}
	}
	raw := cacheResponseFreshness(w.buf.header, w.requested, w.received, w.cache.ttl)
	return raw.constrain(cacheResponseFreshness(projected, w.requested, w.received, w.cache.ttl))
}

// A lease keeps bytes alive; freshness is checked again at replay commitment.
func cacheReplayFresh(w http.ResponseWriter, e *cacheEntry) bool {
	h := e.buf.header.Clone()
	now := time.Now()
	if !now.Before(e.expires) {
		return false
	}
	e.freshness.apply(h, now)
	cacheCommitHeaders(w, h, e.buf.status)
	_, _ = cacheWrite(w, e.buf.body.Bytes())
	cacheFinishTrailers(w, h)
	return true
}

func cacheWrite(w http.ResponseWriter, p []byte) (int, error) {
	n, err := w.Write(p) //nolint:gosec // G705: transparent producer-body delivery, not an HTML construction sink.
	if n != len(p) && err == nil {
		err = io.ErrShortWrite
	}
	return n, err
}

// Replay uses one bounded private header workset. The body stays leased until
// the caller's deferred release, including when the downstream writer panics.
func cacheReplay(w http.ResponseWriter, b *responseBuffer) error {
	h := b.header.Clone()
	cacheCommitHeaders(w, h, b.status)
	_, err := cacheWrite(w, b.body.Bytes())
	cacheFinishTrailers(w, h)
	return err
}

// Trailer scans deliberately allocate no token slices. Even an oversized
// producer-controlled Trailer declaration is safe on the streaming path.
func cacheIsTrailer(name string, h http.Header, declared bool) bool {
	if strings.HasPrefix(name, http.TrailerPrefix) {
		return true
	}
	if !declared {
		return false
	}
	for key, values := range h {
		if !strings.EqualFold(key, "Trailer") {
			continue
		}
		for _, value := range values {
			for token := range strings.SplitSeq(value, ",") {
				if strings.EqualFold(name, strings.TrimSpace(token)) {
					return true
				}
			}
		}
	}
	return false
}

func cacheCommitHeaders(w http.ResponseWriter, h http.Header, status int) {
	dst := w.Header()
	declared := cacheHasTrailerDeclarations(h)
	for name := range dst {
		if cacheIsTrailer(name, h, declared) {
			delete(dst, name)
		}
	}
	for name, values := range h {
		if cacheIsTrailer(name, h, declared) {
			if strings.HasPrefix(name, http.TrailerPrefix) {
				dst[name] = nil
			}
			continue
		}
		dst[name] = values
	}
	w.WriteHeader(status)
}

func cacheFinishTrailers(w http.ResponseWriter, h http.Header) {
	declared := cacheHasTrailerDeclarations(h)
	for name, values := range h {
		if cacheIsTrailer(name, h, declared) {
			w.Header()[name] = values
		}
	}
}

func cacheHasTrailerDeclarations(h http.Header) bool {
	for name, values := range h {
		if len(values) != 0 && strings.EqualFold(name, "Trailer") {
			return true
		}
	}
	return false
}
