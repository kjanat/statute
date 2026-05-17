package statute

import (
	"net/http"
	"sync"
	"time"

	"statute.kjanat.dev/resolved"
)

// cacheHandler is a tiny in-process response cache. It stores 2xx GET/HEAD
// responses for the configured TTL, keyed on the request host + URI. Entries
// are not size-bounded; for production deployments with large response bodies
// or high cardinality, swap this for a real LRU.
func cacheHandler(m resolved.Middleware, next http.Handler) http.Handler {
	ttl := m.CacheTTL
	if ttl <= 0 {
		return next
	}
	c := newTTLCache(ttl)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		key := r.Method + " " + r.Host + r.URL.RequestURI()
		if entry := c.get(key); entry != nil {
			entry.replay(w)
			return
		}
		buf := newResponseBuffer()
		next.ServeHTTP(buf, r)
		if buf.status >= 200 && buf.status < 300 {
			c.put(key, buf)
		}
		buf.replay(w)
	})
}

type ttlCache struct {
	ttl     time.Duration
	mu      sync.RWMutex
	entries map[string]cacheEntry
}

type cacheEntry struct {
	buf     *responseBuffer
	expires time.Time
}

func newTTLCache(ttl time.Duration) *ttlCache {
	return &ttlCache{ttl: ttl, entries: make(map[string]cacheEntry)}
}

func (c *ttlCache) get(key string) *responseBuffer {
	c.mu.RLock()
	e, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok {
		return nil
	}
	if time.Now().After(e.expires) {
		c.mu.Lock()
		delete(c.entries, key)
		c.mu.Unlock()
		return nil
	}
	return e.buf
}

func (c *ttlCache) put(key string, buf *responseBuffer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = cacheEntry{buf: buf, expires: time.Now().Add(c.ttl)}
}
