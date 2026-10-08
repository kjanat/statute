package statute

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"
)

func TestCacheFreshnessInnerBufferPartialTTL(t *testing.T) {
	for _, buffer := range []Middleware{ETag(), Retry(2), Timeout("1m")} {
		t.Run(fmt.Sprintf("%T", buffer), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				stamp := time.Now().UTC().Format(http.TimeFormat)
				h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls++
					w.Header().Set("Age", "10")
					w.WriteHeader(200)
					time.Sleep(time.Second)
					_, _ = io.WriteString(w, "body")
				}), Cache("3s"), buffer)
				first := runRequest(t, h, httptest.NewRequest("GET", "/", nil))
				if first.Header().Get("Age") != "11" || first.Header().Get("Date") != stamp {
					t.Fatalf("hidden buffering missing from age/date: %v", first.Header())
				}
				time.Sleep(time.Second)
				runRequest(t, h, httptest.NewRequest("GET", "/", nil))
				if calls != 1 {
					t.Fatal("fresh entry was not reused")
				}
				time.Sleep(time.Second)
				runRequest(t, h, httptest.NewRequest("GET", "/", nil))
				if calls != 2 {
					t.Fatal("buffered time was not charged against TTL")
				}
			})
		})
	}
}

func TestCacheFreshnessDiscardedAttemptInvalidates(t *testing.T) {
	calls := 0
	h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Cache-Control", "max-age=60")
		if calls%2 == 1 {
			w.WriteHeader(503)
			w.Header().Del("Cache-Control")
			return
		}
		_, _ = io.WriteString(w, "body")
	}), Cache("1h"), Retry(2, OnStatus(503)), ETag(), Cache("1h"))
	for range 2 {
		if got := runRequest(t, h, httptest.NewRequest("GET", "/", nil)); got.Code != 200 || got.Body.String() != "body" {
			t.Fatalf("retry delivery changed: %d %q", got.Code, got.Body.String())
		}
	}
	if calls != 4 {
		t.Fatalf("unsafe discarded attempt allowed retention: calls=%d", calls)
	}
}

func TestCacheFreshnessLateTimeoutObservation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := &cacheObservation{}
		release := make(chan struct{})
		h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cache-Control", "max-age=60")
			w.WriteHeader(200)
			<-release
			w.Header().Del("Cache-Control")
		}), Cache("1h"), Timeout("1s"), ETag())
		r := httptest.NewRequest("GET", "/", nil)
		r = r.WithContext(context.WithValue(r.Context(), cacheObservationKey{}, o))
		if got := runRequest(t, h, r); got.Code != 503 {
			t.Fatalf("timeout status=%d", got.Code)
		}
		close(release)
		synctest.Wait()
		if o.usable() {
			t.Fatal("late producer did not invalidate observation")
		}
	})
}

func TestCacheFreshnessBufferPolicyMutation(t *testing.T) {
	for _, field := range []string{"Age", "Date", "Expires", "Cache-Control"} {
		for _, explicit := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/explicit=%t", field, explicit), func(t *testing.T) {
				calls := 0
				h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls++
					w.Header().Set(field, freshnessMutationValue(field))
					if explicit {
						w.WriteHeader(200)
					}
					_, _ = io.WriteString(w, "body")
					w.Header().Del(field)
				}), Cache("1h"), ETag(), Retry(2))
				for range 2 {
					runRequest(t, h, httptest.NewRequest("GET", "/", nil))
				}
				if calls != 2 {
					t.Fatalf("mutated %s retained: calls=%d", field, calls)
				}
			})
		}
	}
}

func freshnessMutationValue(field string) string {
	switch field {
	case "Age":
		return "20"
	case "Date":
		return time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	case "Expires":
		return time.Now().Add(time.Minute).UTC().Format(http.TimeFormat)
	default:
		return "max-age=60"
	}
}

func TestCacheFreshnessInnerBufferCommit(t *testing.T) {
	for _, buffer := range []Middleware{ETag(), Retry(2), Timeout("1m")} {
		t.Run(fmt.Sprintf("%T", buffer), func(t *testing.T) {
			calls := 0
			h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.Header().Set("Cache-Control", "max-age=0, must-revalidate")
				w.WriteHeader(200)
				_, _ = io.WriteString(w, "body")
				w.Header().Del("Cache-Control")
			}), Cache("1h"), buffer)
			for range 2 {
				runRequest(t, h, httptest.NewRequest("GET", "/", nil))
			}
			if calls != 2 {
				t.Fatalf("inner buffer erased committed freshness: calls=%d", calls)
			}
		})
	}
}

func TestCacheFreshnessInnerCacheOverflowPolicy(t *testing.T) {
	calls := 0
	h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Cache-Control", "max-age=0")
		w.WriteHeader(200)
		w.Header().Del("Cache-Control")
		_, _ = io.WriteString(w, "overflow")
	}), Cache("1h"), ETag(), Cache("1h").MaxResponseBody("1B"))
	for range 2 {
		if got := runRequest(t, h, httptest.NewRequest("GET", "/", nil)); got.Body.String() != "overflow" {
			t.Fatal("overflow delivery changed")
		}
	}
	if calls != 2 {
		t.Fatalf("inner overflow erased committed policy: calls=%d", calls)
	}
}

func TestCacheFreshnessInnerBufferLegacyTTL(t *testing.T) {
	for _, buffer := range []Middleware{ETag(), Retry(2), Timeout("1m")} {
		t.Run(fmt.Sprintf("%T", buffer), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls++
					w.WriteHeader(200)
					time.Sleep(2 * time.Second)
					_, _ = io.WriteString(w, "body")
				}), Cache("1s"), buffer)
				for range 2 {
					runRequest(t, h, httptest.NewRequest("GET", "/", nil))
				}
				if calls != 2 {
					t.Fatalf("inner buffer restarted TTL: calls=%d", calls)
				}
			})
		})
	}
}
