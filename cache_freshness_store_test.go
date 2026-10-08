package statute

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"statute.kjanat.dev/resolved"
)

func TestCacheFreshnessLeaseExpiresBeforeReplay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newTTLCache(time.Second)
		key := cacheKey{target: "/"}
		publishCacheTestEntry(t, c, key, nil, nil, "old")
		e := c.get(key, nil, nil)
		if e == nil {
			t.Fatal("entry missing")
		}
		defer c.release(e)
		time.Sleep(time.Second)
		w := httptest.NewRecorder()
		if cacheReplayFresh(w, e) || w.Body.Len() != 0 || len(w.Header()) != 0 {
			t.Fatal("expired lease replayed")
		}
	})
}

func TestCacheFreshnessConcurrentReplayOwnsHeaders(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newTTLCache(time.Hour)
		e := publishCacheTestEntry(t, c, cacheKey{target: "/"}, nil, nil, "body")
		original := e.buf.header.Clone()
		time.Sleep(2 * time.Second)
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				w := httptest.NewRecorder()
				if !cacheReplayFresh(w, e) || w.Header().Get("Age") != "2" {
					t.Error("incorrect replay age")
				}
				w.Header().Set("Age", "999")
			})
		}
		wg.Wait()
		if !reflect.DeepEqual(e.buf.header, original) {
			t.Fatal("replay mutated retained headers")
		}
	})
}

func TestCacheFreshnessGeneratedHeadersCountTowardLimit(t *testing.T) {
	for _, names := range []int{cacheHeaderNames - 2, cacheHeaderNames - 1} {
		t.Run(fmt.Sprint(names), func(t *testing.T) {
			calls := 0
			h := cacheHandler(resolved.Middleware{CacheTTL: time.Hour}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				for i := range names {
					w.Header().Set(fmt.Sprintf("X-%d", i), "v")
				}
				_, _ = w.Write([]byte("body"))
			}))
			for range 2 {
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
				if w.Body.String() != "body" {
					t.Fatal("delivery changed")
				}
			}
			want := 1
			if names+2 > cacheHeaderNames {
				want = 2
			}
			if calls != want {
				t.Fatalf("calls=%d, want %d", calls, want)
			}
		})
	}
}
