package statute

import (
	"net/http"
	"slices"
	"sync"
	"time"

	"statute.kjanat.dev/resolved"
)

// cacheHandler stores permitted full GET/HEAD responses by method, host, URI,
// and Vary. Entries expire by TTL; their count and body sizes are unbounded.
func cacheHandler(m resolved.Middleware, next http.Handler) http.Handler {
	ttl := m.CacheTTL
	if ttl <= 0 {
		return next
	}
	c := newTTLCache(ttl)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !cacheRequestEligible(r) {
			next.ServeHTTP(w, r)
			return
		}
		key := r.Method + " " + r.Host + r.URL.RequestURI()
		if entry := c.get(key, r.Header); entry != nil {
			entry.replay(w)
			return
		}
		requestHeaders := r.Header.Clone()
		requestAllowsStorage := cacheControlAllowsStorage(requestHeaders)
		buf := newResponseBuffer()
		next.ServeHTTP(buf, r)
		projected := responseHeadersForCache(r.Context(), buf.Header())
		vary, reusable := cacheVary(buf.Header(), projected)
		if cacheResponseEligible(buf) && reusable &&
			requestAllowsStorage && cacheResponseAllowsStorage(buf.Header()) &&
			cacheResponseAllowsStorage(projected) {
			c.put(key, requestHeaders, vary, buf)
		}
		buf.replay(w)
	})
}

type ttlCache struct {
	ttl     time.Duration
	mu      sync.Mutex
	entries map[string][]cacheEntry
}

type cacheEntry struct {
	buf     *responseBuffer
	expires time.Time
	vary    []cacheVaryField
}

func newTTLCache(ttl time.Duration) *ttlCache {
	return &ttlCache{ttl: ttl, entries: make(map[string][]cacheEntry)}
}

func (c *ttlCache) get(key string, headers http.Header) *responseBuffer {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	entries := slices.DeleteFunc(c.entries[key], func(e cacheEntry) bool { return !now.Before(e.expires) })
	if len(entries) == 0 {
		delete(c.entries, key)
	} else {
		c.entries[key] = entries
	}
	for _, e := range entries {
		if e.matches(headers) {
			return e.buf
		}
	}
	return nil
}

func (c *ttlCache) put(key string, headers http.Header, names []string, buf *responseBuffer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	entry := cacheEntry{buf: buf, expires: now.Add(c.ttl)}
	for _, name := range names {
		values, present := cacheHeaderValues(headers, name)
		entry.vary = append(entry.vary, cacheVaryField{name: name, values: values, present: present})
	}
	entries := slices.DeleteFunc(c.entries[key], func(e cacheEntry) bool {
		return !now.Before(e.expires) || !e.sameVary(entry) || e.matches(headers)
	})
	c.entries[key] = append(entries, entry)
}
