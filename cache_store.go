package statute

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// A flat intrusive list bounds index memory even under replacement and churn.
// count includes candidates and retired entries still leased by slow readers.
type ttlCache struct {
	mu           sync.Mutex
	ttl          time.Duration
	limit, count int
	maxBody      int64
	budget       *responseBufferBudget
	head, tail   *cacheEntry
}

type cacheEntry struct {
	buf        *responseBuffer
	key        cacheKey
	expires    time.Time
	freshness  cacheFreshness
	vary       []cacheVaryField
	prev, next *cacheEntry
	refs       int
	retained   bool
	charge     int64
}

func newTTLCache(ttl time.Duration) *ttlCache {
	return newBoundedTTLCache(ttl, 0, 0, 0)
}

func newBoundedTTLCache(ttl time.Duration, entries int, body, budget int64) *ttlCache {
	if entries <= 0 {
		entries = defaultCacheMaxEntries
	}
	if body <= 0 {
		body = defaultMaxResponseBodyBytes
	}
	return &ttlCache{ttl: ttl, limit: entries, maxBody: body, budget: newResponseBufferBudget(budget)}
}

// begin reserves scratch before key construction, policy helpers, or copies.
func (c *ttlCache) begin() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked(time.Now())
	return c.reserveLocked(cacheScratchBytes)
}

func (c *ttlCache) end() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.budget.release(cacheScratchBytes)
	c.sweepLocked(time.Now())
}

func (c *ttlCache) reserveLocked(n int64) bool {
	for !c.budget.reserve(n) {
		if c.head == nil {
			return false
		}
		c.retireLocked(c.head)
	}
	return true
}

func (c *ttlCache) sweepLocked(now time.Time) {
	for e := c.head; e != nil; {
		next := e.next
		if !now.Before(e.expires) {
			c.retireLocked(e)
		}
		e = next
	}
}

func (c *ttlCache) candidate() *cacheEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked(time.Now())
	for c.count >= c.limit && c.head != nil {
		c.retireLocked(c.head)
	}
	if c.count >= c.limit || !c.reserveLocked(cacheEntryControlBytes) {
		return nil
	}
	c.count++
	return &cacheEntry{refs: 1, charge: cacheEntryControlBytes}
}

func (c *ttlCache) attach(e *cacheEntry, buf *responseBuffer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked(time.Now())
	e.buf = buf
}

func (c *ttlCache) get(key cacheKey, headers http.Header, policy *cacheProxyPolicy) *cacheEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked(time.Now())
	if policy != nil && policy.unsafe.Load() {
		return nil
	}
	for e := c.head; e != nil; e = e.next {
		if e.key == key && e.allowedBy(policy) && e.matches(headers) {
			e.refs++
			return e
		}
	}
	return nil
}

// publish reserves retained metadata before making owned copies. Body capacity
// was already charged by the candidate writer; it is transferred without copying.
func (c *ttlCache) publish(e *cacheEntry, key cacheKey, headers http.Header, names []string, buf *responseBuffer, policy *cacheProxyPolicy) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked(time.Now())
	e.buf = buf
	if !e.freshness.valid || !time.Now().Before(e.freshness.expires) {
		return false
	}
	charge, ok := cacheMetadataCost(key, headers, names, buf.header)
	if !ok || !policy.allowsVary(names) || !c.reserveLocked(charge) {
		return false
	}
	e.charge += charge
	e.key = cacheCloneKey(key)
	e.vary = cacheSnapshotVary(headers, names)
	owned := *buf
	owned.header = cacheCloneHeader(buf.header)
	e.buf = &owned
	c.replaceVariantsLocked(e, key, headers)
	e.expires = e.freshness.expires
	e.retained = true
	e.refs++
	e.prev = c.tail
	if c.tail == nil {
		c.head = e
	} else {
		c.tail.next = e
	}
	c.tail = e
	return true
}

func (c *ttlCache) replaceVariantsLocked(e *cacheEntry, key cacheKey, headers http.Header) {
	for old := c.head; old != nil; {
		next := old.next
		if old.key == key && (!old.sameVary(*e) || old.matches(headers)) {
			c.retireLocked(old)
		}
		old = next
	}
}

func cacheSnapshotVary(headers http.Header, names []string) []cacheVaryField {
	vary := make([]cacheVaryField, 0, len(names))
	for _, name := range names {
		values, present := cacheHeaderValues(headers, name)
		for i := range values {
			values[i] = strings.Clone(values[i])
		}
		vary = append(vary, cacheVaryField{name: strings.Clone(name), values: values, present: present})
	}
	return vary
}

func (c *ttlCache) release(e *cacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.releaseLocked(e)
	c.sweepLocked(time.Now())
}

func (c *ttlCache) retireLocked(e *cacheEntry) {
	if e.prev == nil {
		c.head = e.next
	} else {
		e.prev.next = e.next
	}
	if e.next == nil {
		c.tail = e.prev
	} else {
		e.next.prev = e.prev
	}
	e.prev, e.next, e.retained = nil, nil, false
	c.releaseLocked(e)
}

func (c *ttlCache) releaseLocked(e *cacheEntry) {
	e.refs--
	if e.refs != 0 {
		return
	}
	if e.buf != nil {
		e.buf.release()
		e.buf = nil
	}
	c.budget.release(e.charge)
	e.vary = nil
	e.key = cacheKey{}
	c.count--
}
