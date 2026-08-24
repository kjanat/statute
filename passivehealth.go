package statute

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"statute.kjanat.dev/resolved"
)

// passiveRun owns one generation of a pool's passive-failure windows. Like
// healthRun, the generation state lives on the returned handle rather than
// the reusable poolHandler, so a stopped generation can neither record into
// nor age out a later generation's windows, and a restart after a failed
// Start begins from empty windows. Aging is computed lazily at record and
// read time; no goroutine runs on the run's behalf.
type passiveRun struct {
	cfg    resolved.PassiveHealthCheck
	now    func() time.Time
	active atomic.Bool

	mu       sync.Mutex
	failures map[*backendState][]time.Time
}

func newPassiveRun(cfg resolved.PassiveHealthCheck, now func() time.Time) *passiveRun {
	r := &passiveRun{
		cfg:      cfg,
		now:      now,
		failures: make(map[*backendState][]time.Time),
	}
	r.active.Store(true)
	return r
}

// startPassive begins a fresh passive generation for the pool, publishing it
// where candidates() and ServeHTTP find it. Every start gets new windows: a
// rolled-back attempt's backends never served real traffic, so nothing it
// observed may survive into the next attempt.
func (ph *poolHandler) startPassive() *passiveRun {
	if !ph.pool.PassiveHealthCheck.Enabled {
		return nil
	}
	run := newPassiveRun(ph.pool.PassiveHealthCheck, ph.now)
	ph.passive.Store(run)
	return run
}

func (r *passiveRun) stop() {
	if r == nil {
		return
	}
	r.active.Store(false)
}

// record adds one backend-attempt failure at the current time. Successes are
// deliberately not recorded: passive recovery happens only by failures aging
// out of the window. A stopped run records nothing, so an attempt that was
// in flight across a generation swap cannot pollute the next generation.
func (r *passiveRun) record(b *backendState) {
	if r == nil || !r.active.Load() {
		return
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.active.Load() {
		return
	}
	kept := pruneWindow(r.failures[b], now.Add(-r.cfg.FailureWindow))
	kept = append(kept, now)
	// Only the most recent MaxFailures timestamps can ever satisfy the
	// demotion predicate, so older ones are dropped to bound memory.
	if len(kept) > r.cfg.MaxFailures {
		kept = kept[len(kept)-r.cfg.MaxFailures:]
	}
	r.failures[b] = kept
}

// demoted reports whether b has accumulated MaxFailures failures inside the
// sliding window, aging expired entries out as it reads.
func (r *passiveRun) demoted(b *backendState) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := pruneWindow(r.failures[b], r.now().Add(-r.cfg.FailureWindow))
	r.failures[b] = kept
	return len(kept) >= r.cfg.MaxFailures
}

// pruneWindow drops the timestamps at or before cutoff. Entries are
// append-ordered, so the survivors are always a suffix.
func pruneWindow(ts []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(ts) && !ts[i].After(cutoff) {
		i++
	}
	return ts[i:]
}

type passiveRunContextKey struct{}

// withPassiveRun pins the passive generation an attempt records into. The
// backend proxies' outcome hooks read it back from the outbound request's
// context, so a request that outlives its generation records into that
// stopped — and therefore inert — run instead of a successor's windows.
func withPassiveRun(ctx context.Context, run *passiveRun) context.Context {
	return context.WithValue(ctx, passiveRunContextKey{}, run)
}

func passiveRunFromContext(ctx context.Context) *passiveRun {
	run, _ := ctx.Value(passiveRunContextKey{}).(*passiveRun)
	return run
}
