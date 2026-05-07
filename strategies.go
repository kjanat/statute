package statute

import (
	"hash/fnv"
	"net/http/httputil"
	"sync"
	"sync/atomic"

	"github.com/kjanat/statute/resolved"
)

// backendState carries runtime state for a single backend: in-flight counter,
// health flag, and a dedicated reverse proxy bound to its URL.
type backendState struct {
	backend  *resolved.Backend
	rp       *httputil.ReverseProxy
	inFlight atomic.Int64
	healthy  atomic.Bool
}

func (b *backendState) markHealthy(v bool) { b.healthy.Store(v) }
func (b *backendState) isHealthy() bool    { return b.healthy.Load() }

// healthy returns the subset of states whose healthy flag is set.
func healthy(states []*backendState) []*backendState {
	out := make([]*backendState, 0, len(states))
	for _, s := range states {
		if s.isHealthy() {
			out = append(out, s)
		}
	}
	return out
}

// picker selects a backend per the pool's strategy from the given candidate
// set. Pickers receive a stable, ordered slice of healthy candidates.
type picker interface {
	pick(candidates []*backendState, key string) *backendState
}

func newPicker(strategy resolved.Strategy) picker {
	switch strategy {
	case resolved.RoundRobin:
		return &roundRobinPicker{}
	case resolved.LeastConnections:
		return &leastConnPicker{}
	case resolved.IPHash:
		return &ipHashPicker{}
	case resolved.Weighted:
		return &weightedPicker{}
	default:
		return &roundRobinPicker{}
	}
}

type roundRobinPicker struct{ idx atomic.Uint64 }

func (p *roundRobinPicker) pick(c []*backendState, _ string) *backendState {
	if len(c) == 0 {
		return nil
	}
	i := p.idx.Add(1) - 1
	return c[i%uint64(len(c))]
}

type leastConnPicker struct{}

func (*leastConnPicker) pick(c []*backendState, _ string) *backendState {
	if len(c) == 0 {
		return nil
	}
	best := c[0]
	bestLoad := best.inFlight.Load()
	for _, s := range c[1:] {
		l := s.inFlight.Load()
		if l < bestLoad {
			best, bestLoad = s, l
		}
	}
	return best
}

type ipHashPicker struct{}

func (*ipHashPicker) pick(c []*backendState, key string) *backendState {
	if len(c) == 0 {
		return nil
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return c[h.Sum64()%uint64(len(c))]
}

// weightedPicker implements smooth weighted round-robin (Nginx-style). It
// minimises the variance between selections relative to weights, unlike
// integer-modulo weighted RR.
type weightedPicker struct {
	mu      sync.Mutex
	current map[*backendState]int
}

func (p *weightedPicker) pick(c []*backendState, _ string) *backendState {
	if len(c) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current == nil {
		p.current = make(map[*backendState]int)
	}

	total := 0
	var best *backendState
	for _, s := range c {
		w := s.backend.Weight
		if w <= 0 {
			w = 1
		}
		p.current[s] += w
		total += w
		if best == nil || p.current[s] > p.current[best] {
			best = s
		}
	}
	if best != nil {
		p.current[best] -= total
	}
	return best
}
