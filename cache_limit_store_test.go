package statute

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func publishCacheTestEntry(t *testing.T, c *ttlCache, key cacheKey, h http.Header, vary []string, body string) *cacheEntry {
	t.Helper()
	if !c.begin() {
		t.Fatal("scratch reservation failed")
	}
	defer c.end()
	e := c.candidate()
	if e == nil {
		t.Fatal("entry reservation failed")
	}
	defer c.release(e)
	buf := newLimitedResponseBuffer(c.maxBody)
	buf.budget = c.budget
	c.attach(e, buf)
	if _, err := buf.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	buf.header.Set("X-Original", "owned")
	now := time.Now()
	e.freshness = cacheResponseFreshness(buf.header, now, now, c.ttl)
	if !c.publish(e, key, h, vary, buf, nil) {
		t.Fatal("entry publication failed")
	}
	return e
}

func getCacheTestEntry(c *ttlCache, key cacheKey, h http.Header) *cacheEntry {
	e := c.get(key, h, nil)
	if e != nil {
		c.release(e)
	}
	return e
}

func cacheStoreUsage(c *ttlCache) (int, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.budget.mu.Lock()
	defer c.budget.mu.Unlock()
	return c.count, c.budget.used
}

func TestCacheStoreEntryAndVariantBounds(t *testing.T) {
	for _, variants := range []bool{false, true} {
		c := newBoundedTTLCache(time.Hour, 3, 1024, 4<<20)
		for i := range 20 {
			key := cacheKey{target: fmt.Sprint(i)}
			var h http.Header
			var vary []string
			if variants {
				key.target = "shared"
				h = http.Header{"X-Variant": {fmt.Sprint(i)}}
				vary = []string{"x-variant"}
			}
			publishCacheTestEntry(t, c, key, h, vary, "body")
			count, used := cacheStoreUsage(c)
			if count > 3 || used > 4<<20 {
				t.Fatalf("count=%d used=%d", count, used)
			}
		}
		if count, _ := cacheStoreUsage(c); count != 3 {
			t.Fatal(count)
		}
	}
}

func TestCacheStoreExpiryAcrossKeys(t *testing.T) {
	c := newTTLCache(time.Hour)
	a := publishCacheTestEntry(t, c, cacheKey{target: "a"}, nil, nil, "one")
	b := publishCacheTestEntry(t, c, cacheKey{target: "b"}, nil, nil, "two")
	_, before := cacheStoreUsage(c)
	a.expires = time.Now().Add(-time.Second)
	if getCacheTestEntry(c, cacheKey{target: "b"}, nil) != b {
		t.Fatal("live unrelated entry lost")
	}
	count, after := cacheStoreUsage(c)
	if count != 1 || after >= before || a.buf != nil {
		t.Fatalf("expired owner retained: count=%d before=%d after=%d", count, before, after)
	}
}

func TestCacheStoreRetiredReaderKeepsCharge(t *testing.T) {
	c := newBoundedTTLCache(time.Hour, 1, 1024, 2<<20)
	key := cacheKey{target: "a"}
	e := publishCacheTestEntry(t, c, key, nil, nil, "leased")
	reader := c.get(key, nil, nil)
	if reader != e {
		t.Fatal("missing reader")
	}
	_, before := cacheStoreUsage(c)
	c.mu.Lock()
	c.retireLocked(e)
	c.mu.Unlock()
	count, after := cacheStoreUsage(c)
	if count != 1 || after != before || reader.buf.body.String() != "leased" {
		t.Fatalf("retirement released reader: count=%d before=%d after=%d", count, before, after)
	}
	if candidate := c.candidate(); candidate != nil {
		c.release(candidate)
		t.Fatal("retired reader did not count against capacity")
	}
	c.release(reader)
	if count, used := cacheStoreUsage(c); count != 0 || used != 0 {
		t.Fatalf("released count=%d bytes=%d", count, used)
	}
	candidate := c.candidate()
	if candidate == nil {
		t.Fatal("capacity not restored")
	}
	c.release(candidate)
}

func TestCacheStoreConcurrentEmptyCandidates(t *testing.T) {
	c := newBoundedTTLCache(time.Hour, 4, 16, 64<<20)
	var wg sync.WaitGroup
	entries := make(chan *cacheEntry, 32)
	for range 32 {
		wg.Go(func() { entries <- c.candidate() })
	}
	wg.Wait()
	close(entries)
	count, used := cacheStoreUsage(c)
	if count != 4 || used <= 0 {
		t.Fatalf("count=%d used=%d", count, used)
	}
	for e := range entries {
		if e != nil {
			c.release(e)
		}
	}
	if count, used = cacheStoreUsage(c); count != 0 || used != 0 {
		t.Fatalf("count=%d used=%d", count, used)
	}
}

func TestCacheStoreScratchBudget(t *testing.T) {
	c := newBoundedTTLCache(time.Hour, 10, 1024, cacheScratchBytes)
	if !c.begin() || c.begin() {
		t.Fatal("scratch admission exceeded budget")
	}
	c.end()
	if _, used := cacheStoreUsage(c); used != 0 {
		t.Fatalf("scratch leak=%d", used)
	}
}

type cacheSlowWriter struct {
	*httptest.ResponseRecorder
	entered, release chan struct{}
	once             sync.Once
}

func (w *cacheSlowWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered); <-w.release })
	return w.ResponseRecorder.Write(p)
}

func TestCacheGrowthFallbackHoldsPrefixCharge(t *testing.T) {
	c := newBoundedTTLCache(time.Hour, 1, 1024, cacheScratchBytes+cacheEntryControlBytes+1024)
	e := beginCacheTestCandidate(t, c)
	w := &cacheSlowWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	writer := newCacheWriter(w, c, e)
	if _, err := writer.Write([]byte(strings.Repeat("x", 512))); err != nil {
		t.Fatal(err)
	}
	checkCachePrefixRetention(t, c, writer, w)
	c.release(e)
	c.end()
	if w.Code != 200 || w.Body.String() != strings.Repeat("x", 512)+"y" {
		t.Fatalf("stream changed: status=%d length=%d", w.Code, w.Body.Len())
	}
	if count, used := cacheStoreUsage(c); count != 0 || used != 0 {
		t.Fatalf("count=%d used=%d", count, used)
	}
}

func beginCacheTestCandidate(t *testing.T, c *ttlCache) *cacheEntry {
	t.Helper()
	if !c.begin() {
		t.Fatal("scratch refused")
	}
	e := c.candidate()
	if e == nil {
		t.Fatal("candidate refused")
	}
	return e
}

func checkCachePrefixRetention(t *testing.T, c *ttlCache, writer *cacheWriter, w *cacheSlowWriter) {
	t.Helper()
	_, before := cacheStoreUsage(c)
	done := make(chan error, 1)
	go func() { _, err := writer.Write([]byte("y")); done <- err }()
	<-w.entered
	count, during := cacheStoreUsage(c)
	// The old body allocation remains live while its bytes are in Write.
	if count != 1 || during != before {
		t.Errorf("prefix released early: count=%d before=%d during=%d", count, before, during)
	}
	if c.begin() {
		c.end()
		t.Error("scratch capacity exceeded during slow prefix")
	}
	close(w.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
