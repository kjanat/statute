package statute

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"statute.kjanat.dev/resolved"
)

func TestRequestIDConcurrentPublication(t *testing.T) {
	var holder ridHolder
	var wg sync.WaitGroup
	for _, id := range []string{"short", strings.Repeat("long", 100)} {
		wg.Go(func() {
			for range 1000 {
				holder.store(id)
			}
		})
	}
	for range 1000 {
		if id := holder.load(); id != "" && id != "short" && id != strings.Repeat("long", 100) {
			t.Errorf("torn publication: %q", id)
		}
	}
	wg.Wait()
}

func TestRequestIDPrivateHeadersPreserveLiveBody(t *testing.T) {
	for _, output := range []string{defaultRequestIDHeader, "User-Agent", "Referer", "X-Custom"} {
		t.Run(output, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/", strings.NewReader("body"))
			r.Header.Set(output, "original")
			r.Header.Set("X-Input", "selected")
			r.Trailer = http.Header{"X-Trailer": nil}
			h := requestIDHandler(resolved.Middleware{RequestIDHeader: output, RequestIDFromHeader: "X-Input"},
				http.HandlerFunc(func(_ http.ResponseWriter, got *http.Request) {
					if got == r || got.Body != r.Body || got.URL != r.URL {
						t.Error("expected private request with shared body and URL")
					}
					if got.Header.Get(output) != "selected" || r.Header.Get(output) != "original" {
						t.Error("header ownership was not isolated")
					}
					body, err := io.ReadAll(got.Body)
					if err != nil || string(body) != "body" {
						t.Fatalf("body=%q err=%v", body, err)
					}
					r.Trailer.Set("X-Trailer", "at-eof")
					if got.Trailer.Get("X-Trailer") != "at-eof" {
						t.Error("live trailer map was copied")
					}
				}))
			runRequest(t, h, r)
		})
	}
}

func TestRequestIDTimeoutLatePublication(t *testing.T) {
	var log bytes.Buffer
	entered, release, produced := make(chan struct{}), make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	id := requestIDHandler(resolved.Middleware{RequestIDFromHeader: "X-Input"}, noContentHandler)
	late := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(produced)
		close(entered)
		<-release
		id.ServeHTTP(w, r)
	})
	timed := newTimeoutHandler(resolved.Middleware{Timeout: time.Hour}, late)
	h := accessLogMiddleware(resolved.AccessLog{Enabled: true, Writer: &log, SampleRate: 1}, timed)
	r := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
	r.Header.Set("X-Input", "late-id")
	rec := httptest.NewRecorder()
	returned := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, r)
		close(returned)
	}()
	<-entered
	cancel()
	<-returned
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d", rec.Code)
	}
	before := log.String()
	var entry map[string]any
	if err := json.Unmarshal(log.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if _, ok := entry["request_id"]; ok {
		t.Fatal("ID was logged before publication")
	}
	unblock()
	<-produced
	if log.String() != before || r.Header.Get(defaultRequestIDHeader) != "" {
		t.Fatal("late producer changed caller headers or completed log")
	}
}

func TestRequestIDRetryReadsOriginalInput(t *testing.T) {
	for _, inbound := range []string{"", "client-id"} {
		t.Run(inbound, func(t *testing.T) {
			var ids []string
			h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ids = append(ids, r.Header.Get(defaultRequestIDHeader))
				w.WriteHeader(http.StatusServiceUnavailable)
			}), Retry(2, OnStatus(503)), RequestID().From(defaultRequestIDHeader))
			r := httptest.NewRequest("GET", "/", nil)
			if inbound != "" {
				r.Header.Set(defaultRequestIDHeader, inbound)
			}
			runRequest(t, h, r)
			if len(ids) != 2 || ids[0] == "" || (ids[0] == ids[1]) != (inbound != "") {
				t.Fatalf("input=%q attempts=%v", inbound, ids)
			}
			if r.Header.Get(defaultRequestIDHeader) != inbound {
				t.Fatal("Retry input was overwritten")
			}
		})
	}
}

func TestRequestIDOverlappingTimeoutRetries(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var produced sync.WaitGroup
	produced.Add(2)
	ids := make(chan string, 2)
	id := requestIDHandler(resolved.Middleware{}, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		ids <- requestIDFromContext(r.Context())
	}))
	late := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer produced.Done()
		<-release
		id.ServeHTTP(w, r)
	})
	var log bytes.Buffer
	h := accessLogMiddleware(resolved.AccessLog{Enabled: true, Writer: &log, SampleRate: 1},
		chain(t, late, Retry(2, OnStatus(503)), Timeout("1ms")))
	r := httptest.NewRequest("GET", "/", nil)
	if rec := runRequest(t, h, r); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d", rec.Code)
	}
	unblock()
	produced.Wait()
	first, second := <-ids, <-ids
	if first == "" || second == "" || first == second || r.Header.Get(defaultRequestIDHeader) != "" {
		t.Fatalf("attempt IDs=%q/%q caller headers=%v", first, second, r.Header)
	}
}
