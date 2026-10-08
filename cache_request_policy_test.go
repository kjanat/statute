package statute

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"statute.kjanat.dev/resolved"
)

func TestCacheRetryListenerObservation(t *testing.T) {
	for _, cacheOutside := range []bool{false, true} {
		t.Run(fmt.Sprint(cacheOutside), func(t *testing.T) {
			calls := 0
			origin := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				if calls == 1 {
					http.Error(w, "retry", http.StatusServiceUnavailable)
					return
				}
				_, _ = io.WriteString(w, "public")
			})
			mws := []Middleware{Retry(2, OnStatus(503)), Cache("1h")}
			if cacheOutside {
				mws[0], mws[1] = mws[1], mws[0]
			}
			mws = append([]Middleware{RequestID().From("X-Input")}, mws...)
			var logs bytes.Buffer
			s := &server{cfg: &resolved.Config{}, stats: newStats()}
			s.cfg.Observability.AccessLog = resolved.AccessLog{Enabled: true, Format: "json", Writer: &logs, SampleRate: 1}
			h := s.buildListenerHandler(&resolved.Listener{Scheme: schemeHTTP}, chain(t, origin, mws...), nil)
			for _, id := range []string{"first", "second", "third"} {
				r := httptest.NewRequest(http.MethodGet, "/", nil)
				r.Header.Set("X-Input", id)
				rec := runRequest(t, h, r)
				if rec.Code != http.StatusOK || rec.Body.String() != "public" || rec.Header().Get(defaultRequestIDHeader) != id {
					t.Fatalf("id=%s: status=%d headers=%v body=%q", id, rec.Code, rec.Header(), rec.Body.String())
				}
			}
			assertCacheRequestCounts(t, calls, s.stats)
			assertCacheRequestLogs(t, logs.Bytes())
		})
	}
}

func assertCacheRequestCounts(t *testing.T, calls int, stats *stats) {
	t.Helper()
	if calls != 2 || stats.requests.Load() != 3 || stats.responseBytes.Load() != 18 {
		t.Fatalf("origin=%d observations=%d bytes=%d", calls, stats.requests.Load(), stats.responseBytes.Load())
	}
	var metrics bytes.Buffer
	stats.WritePrometheus(&metrics)
	if !strings.Contains(metrics.String(), "statute_requests_by_status_total{status=\"200\"} 3\n") ||
		strings.Contains(metrics.String(), "status=\"503\"") {
		t.Fatalf("intermediate attempt affected final metrics: %s", metrics.String())
	}
}

func TestCacheRetrySuccessfulStatusSelection(t *testing.T) {
	for _, cacheOutside := range []bool{false, true} {
		t.Run(fmt.Sprint(cacheOutside), func(t *testing.T) {
			calls := 0
			origin := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				_, _ = fmt.Fprintf(w, "attempt-%d", calls)
			})
			mws := []Middleware{Retry(2, OnStatus(200)), Cache("1h")}
			wantCalls := 1
			if cacheOutside {
				mws[0], mws[1] = mws[1], mws[0]
				wantCalls = 2
			}
			h := chain(t, origin, mws...)
			for range 2 {
				rec := runRequest(t, h, httptest.NewRequest(http.MethodGet, "/", nil))
				if rec.Code != http.StatusOK || rec.Body.String() != fmt.Sprintf("attempt-%d", wantCalls) {
					t.Fatalf("response=%d %q", rec.Code, rec.Body.String())
				}
			}
			if calls != wantCalls {
				t.Fatalf("origin calls=%d want=%d", calls, wantCalls)
			}
		})
	}
}

func assertCacheRequestLogs(t *testing.T, logs []byte) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(logs))
	for _, id := range []string{"first", "second", "third"} {
		var entry struct {
			RequestID string `json:"request_id"`
			Status    int    `json:"status"`
		}
		if err := decoder.Decode(&entry); err != nil {
			t.Fatal(err)
		}
		if entry.RequestID != id || entry.Status != http.StatusOK {
			t.Fatalf("id=%s log=%+v", id, entry)
		}
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("extra observation: %v, %v", extra, err)
	}
}

func TestCacheCustomAuthorizationRevocation(t *testing.T) {
	for _, mode := range []string{"cached-handler-unsafe-control", "omit-cache", "no-store-from-first-response", "authorization-header"} {
		t.Run(mode, func(t *testing.T) {
			allowed, calls := true, 0
			auth := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				if !allowed {
					http.Error(w, "revoked", http.StatusForbidden)
					return
				}
				if mode == "no-store-from-first-response" {
					w.Header().Set("Cache-Control", "no-store")
				}
				_, _ = io.WriteString(w, "protected")
			})
			var mws []Middleware
			if mode != "omit-cache" {
				mws = []Middleware{Cache("1h")}
			}
			h := chain(t, auth, mws...)
			request := func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "/", nil)
				if mode == "authorization-header" {
					r.Header.Set("Authorization", "Bearer opaque")
				}
				return r
			}
			if rec := runRequest(t, h, request()); rec.Code != http.StatusOK {
				t.Fatalf("initial status=%d", rec.Code)
			}
			allowed = false
			want, wantCalls := http.StatusForbidden, 2
			if mode == "cached-handler-unsafe-control" {
				want, wantCalls = http.StatusOK, 1
			}
			rec := runRequest(t, h, request())
			if rec.Code != want || calls != wantCalls {
				t.Fatalf("after revocation: status=%d calls=%d; want %d/%d", rec.Code, calls, want, wantCalls)
			}
		})
	}
}
