package statute

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"time"

	"statute.kjanat.dev/resolved"
)

// cacheHandler stores permitted full GET/HEAD responses by request identity and
// Vary. Entries expire by TTL; their count and body sizes are unbounded.
func cacheHandler(m resolved.Middleware, next http.Handler) http.Handler {
	ttl := m.CacheTTL
	if ttl <= 0 {
		return next
	}
	c := newTTLCache(ttl)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		policy := cacheProxyPolicyFromContext(r.Context())
		if !cacheRequestEligible(r) || !policy.requestEligible(r) {
			next.ServeHTTP(w, r)
			return
		}
		key := cacheKeyForRequest(r, policy)
		if entry := c.get(key, r.Header, policy); entry != nil && policy.usable() {
			entry.replay(w)
			return
		}
		requestHeaders := r.Header.Clone()
		requestAllowsStorage := cacheControlAllowsStorage(requestHeaders)
		buf := newResponseBuffer()
		next.ServeHTTP(buf, r)
		if vary, allowed := cacheStorageVary(r.Context(), buf, requestAllowsStorage, policy); allowed {
			c.put(key, requestHeaders, vary, buf, policy)
		}
		buf.replay(w)
	})
}

func cacheStorageVary(ctx context.Context, buf *responseBuffer, requestAllowsStorage bool, policy *cacheProxyPolicy) ([]string, bool) {
	projected := responseHeadersForCache(ctx, buf.Header())
	vary, reusable := cacheVary(buf.Header(), projected)
	return vary, cacheResponseEligible(buf) && reusable && policy.usable() &&
		requestAllowsStorage && cacheResponseAllowsStorage(buf.Header()) &&
		cacheResponseAllowsStorage(projected)
}

// Keep original and effective targets separate: Handle may inspect RequestURI
// after hoisted rewrites change URL. A typed key also avoids delimiter aliases.
type cacheKey struct {
	method, host, target, originalURI, scheme, propagationFields string
	tls                                                          bool
}

func cacheKeyForRequest(r *http.Request, policy *cacheProxyPolicy) cacheKey {
	key := cacheKey{
		method: r.Method, host: r.Host, target: r.URL.RequestURI(),
		originalURI: r.RequestURI, scheme: r.URL.Scheme, tls: r.TLS != nil,
	}
	if policy != nil {
		key.propagationFields = policy.signature
	}
	return key
}

type ttlCache struct {
	ttl     time.Duration
	mu      sync.Mutex
	entries map[cacheKey][]cacheEntry
}

type cacheEntry struct {
	buf     *responseBuffer
	expires time.Time
	vary    []cacheVaryField
}

func newTTLCache(ttl time.Duration) *ttlCache {
	return &ttlCache{ttl: ttl, entries: make(map[cacheKey][]cacheEntry)}
}

func (c *ttlCache) get(key cacheKey, headers http.Header, policy *cacheProxyPolicy) *responseBuffer {
	c.mu.Lock()
	defer c.mu.Unlock()
	if policy != nil && policy.unsafe.Load() {
		return nil
	}
	now := time.Now()
	entries := slices.DeleteFunc(c.entries[key], func(e cacheEntry) bool { return !now.Before(e.expires) })
	if len(entries) == 0 {
		delete(c.entries, key)
	} else {
		c.entries[key] = entries
	}
	for _, e := range entries {
		if e.allowedBy(policy) && e.matches(headers) {
			return e.buf
		}
	}
	return nil
}

func (c *ttlCache) put(key cacheKey, headers http.Header, names []string, buf *responseBuffer, policy *cacheProxyPolicy) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !policy.allowsVary(names) {
		return
	}
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
