package statute

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"statute.kjanat.dev/resolved"
)

func TestRequestIDOwnsCachedOriginEcho(t *testing.T) {
	t.Parallel()
	for order, tail := range [][]Middleware{
		{Cache("1h")}, {ETag(), Cache("1h")}, {Retry(2), ETag(), Cache("1h")},
		{Cache("1h"), ETag(), Retry(2)},
	} {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			t.Parallel()
			var log bytes.Buffer
			calls := 0
			mws := append([]Middleware{RequestID()}, tail...)
			h := accessLogMiddleware(resolved.AccessLog{Enabled: true, Format: "json", Writer: &log, SampleRate: 1},
				chain(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					w.Header().Set(defaultRequestIDHeader, r.Header.Get(defaultRequestIDHeader))
					w.Header()["x-request-id"] = []string{"stale-lowercase"}
					_, _ = io.WriteString(w, "same representation")
				}), mws...))
			ids := make([]string, 0, 3)
			for range 3 {
				log.Reset()
				rec := runRequest(t, h, httptest.NewRequest("GET", "/", nil))
				id := rec.Result().Header.Get(defaultRequestIDHeader)
				if id == "" || slices.Contains(ids, id) {
					t.Fatalf("replayed ID %q; earlier=%v", id, ids)
				}
				ids = append(ids, id)
				assertOnlyRequestID(t, rec.Result().Header, defaultRequestIDHeader, id)
				var entry map[string]any
				if err := json.Unmarshal(log.Bytes(), &entry); err != nil {
					t.Fatal(err)
				}
				if entry["request_id"] != id {
					t.Fatalf("response=%s log=%v", id, entry)
				}
			}
			if calls != 1 {
				t.Fatalf("cache not reused: calls=%d", calls)
			}
		})
	}
}

func assertOnlyRequestID(t *testing.T, h http.Header, name, id string) {
	t.Helper()
	values, present := cacheHeaderValues(h, name)
	if !present || !slices.Equal(values, []string{id}) {
		t.Fatalf("identity %s=%v want [%s]", name, values, id)
	}
}

func TestRequestIDFinalWriterPaths(t *testing.T) {
	t.Parallel()
	for name, commit := range map[string]func(http.ResponseWriter){
		"empty":    func(http.ResponseWriter) {},
		"status":   func(w http.ResponseWriter) { w.WriteHeader(http.StatusBadGateway) },
		"write":    func(w http.ResponseWriter) { _, _ = io.WriteString(w, "body") },
		"flush":    func(w http.ResponseWriter) { _ = http.NewResponseController(w).Flush() },
		"readfrom": func(w http.ResponseWriter) { _, _ = w.(io.ReaderFrom).ReadFrom(strings.NewReader("body")) },
		"informational": func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusEarlyHints)
			clear(w.Header())
			w.WriteHeader(http.StatusOK)
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header()["x-trace"] = []string{"origin"}
				w.Header().Set("X-Trace", "origin")
				commit(w)
			}), RequestID().Header("X-Trace").From("X-Input"))
			r := httptest.NewRequest("GET", "/", nil)
			r.Header.Set("X-Input", "current")
			rec := runRequest(t, h, r)
			assertOnlyRequestID(t, rec.Header(), "X-Trace", "current")
		})
	}
}

func TestRequestIDRawResponsePrecedence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		op   Middleware
		want []string
	}{
		{SetResponseHeader("X-Trace", "route"), []string{"route"}},
		{AddResponseHeader("X-Trace", "route"), []string{"current", "route"}},
		{RemoveResponseHeader("X-Trace"), nil},
	} {
		for _, outer := range []bool{false, true} {
			for _, body := range []bool{false, true} {
				mws := []Middleware{RequestID().Header("X-Trace").From("X-Input"), tc.op}
				if outer {
					slices.Reverse(mws)
				}
				h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("X-Trace", "origin")
					if body {
						_, _ = io.WriteString(w, "body")
						w.Header().Set("X-Trace", "late-origin")
					}
				}), mws...)
				r := httptest.NewRequest("GET", "/", nil)
				r.Header.Set("X-Input", "current")
				if got := runRequest(t, h, r).Header().Values("X-Trace"); !slices.Equal(got, tc.want) {
					t.Fatalf("outer=%v body=%v got=%v want=%v", outer, body, got, tc.want)
				}
			}
		}
	}
}

func TestRequestIDRetryAttemptOwnership(t *testing.T) {
	t.Parallel()
	for _, retryOutside := range []bool{false, true} {
		mws := []Middleware{RequestID(), Retry(2, OnStatus(503))}
		if retryOutside {
			slices.Reverse(mws)
		}
		var ids []string
		h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ids = append(ids, r.Header.Get(defaultRequestIDHeader))
			w.Header().Set(defaultRequestIDHeader, "origin")
			if len(ids) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
		}), mws...)
		r, holder := installRIDHolder(httptest.NewRequest("GET", "/", nil))
		rec := runRequest(t, h, r)
		if len(ids) != 2 || rec.Code != http.StatusOK {
			t.Fatalf("attempts=%v status=%d", ids, rec.Code)
		}
		if (ids[0] != ids[1]) != retryOutside || holder.id != ids[1] {
			t.Fatalf("retryOutside=%v ids=%v holder=%s", retryOutside, ids, holder.id)
		}
		assertOnlyRequestID(t, rec.Result().Header, defaultRequestIDHeader, ids[1])
	}
}

func TestRequestIDHeadConditionalOwnership(t *testing.T) {
	t.Parallel()
	h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Trace", "origin")
		_, _ = io.WriteString(w, "representation")
	}), RequestID().Header("X-Trace").From("X-Input"), ETag(), Cache("1h"))
	get := runRequest(t, h, httptest.NewRequest("GET", "/", nil))
	for _, conditional := range []bool{false, true} {
		r := httptest.NewRequest("HEAD", "/", nil)
		r.Header.Set("X-Input", "head-id")
		want := http.StatusOK
		if conditional {
			r.Header.Set("If-None-Match", get.Header().Get("ETag"))
			want = http.StatusNotModified
		}
		rec := runRequest(t, h, r)
		if rec.Code != want || rec.Body.Len() != 0 {
			t.Fatalf("conditional=%v status=%d body=%q", conditional, rec.Code, rec.Body.String())
		}
		assertOnlyRequestID(t, rec.Result().Header, "X-Trace", "head-id")
	}
}

func TestRequestIDInformationalWireResponse(t *testing.T) {
	t.Parallel()
	h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusEarlyHints)
		clear(w.Header())
		w.Header()["x-trace"] = []string{"origin"}
		_, _ = io.WriteString(w, "body")
	}), RequestID().Header("X-Trace").From("X-Input"))
	srv := httptest.NewServer(h)
	defer srv.Close()
	r, err := http.NewRequestWithContext(t.Context(), "GET", srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("X-Input", "wire-id")
	resp, err := srv.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.Status)
	}
	assertOnlyRequestID(t, resp.Header, "X-Trace", "wire-id")
}

func TestRequestIDDoesNotCommitAfterAbortOrHijack(t *testing.T) {
	t.Parallel()
	for _, hijack := range []bool{false, true} {
		rec := &hijackableRecorder{ResponseRecorder: httptest.NewRecorder()}
		h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set(defaultRequestIDHeader, "origin")
			if hijack {
				if _, _, err := http.NewResponseController(w).Hijack(); err != nil {
					t.Error(err)
				}
				return
			}
			panic(http.ErrAbortHandler)
		}), RequestID())
		var panicValue any
		func() {
			defer func() { panicValue = recover() }()
			h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		}()
		err, isError := panicValue.(error)
		if !hijack && (!isError || !errors.Is(err, http.ErrAbortHandler)) {
			t.Fatalf("abort changed: %v", panicValue)
		}
		if rec.Header().Get(defaultRequestIDHeader) != "origin" {
			t.Fatal("response mutated after ownership ended")
		}
	}
}

func TestRequestIDOwnsTrailerBoundary(t *testing.T) {
	t.Parallel()
	for _, h2 := range []bool{false, true} {
		for _, latePrefix := range []bool{false, true} {
			t.Run(fmt.Sprintf("h2=%v/late=%v", h2, latePrefix), func(t *testing.T) {
				checkRequestIDTrailerBoundary(t, h2, latePrefix, nil)
			})
		}
	}
}

func requestIDTrailerProducer(latePrefix bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Trailer", "X-Finished")
		if !latePrefix {
			w.Header().Add("Trailer", "x-request-id, X-Request-ID")
			w.Header()["trailer"] = []string{"X-REQUEST-ID"}
		}
		_, _ = io.WriteString(w, "body")
		_ = http.NewResponseController(w).Flush()
		w.Header().Set("X-Finished", "done")
		name := defaultRequestIDHeader
		if latePrefix {
			name = http.TrailerPrefix + name
		}
		w.Header().Set(name, "stale-origin-id")
	})
}

func checkRequestIDTrailerBoundary(t *testing.T, h2, latePrefix bool, outer []Middleware) {
	t.Helper()
	mws := append(slices.Clone(outer), RequestID().From("X-Input"))
	owned := chain(t, requestIDTrailerProducer(latePrefix), mws...)
	control := chain(t, requestIDTrailerProducer(latePrefix), outer...)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/control" {
			control.ServeHTTP(w, r)
			return
		}
		owned.ServeHTTP(w, r)
	}))
	srv.EnableHTTP2 = h2
	srv.StartTLS()
	defer srv.Close()
	baseline := requestIDWireResponse(t, srv, "/control")
	resp := requestIDWireResponse(t, srv, "/owned")
	if (resp.ProtoMajor == 2) != h2 {
		t.Fatalf("unexpected protocol %s", resp.Proto)
	}
	assertOnlyRequestID(t, resp.Header, defaultRequestIDHeader, "current-id")
	if _, found := cacheHeaderValues(resp.Trailer, defaultRequestIDHeader); found {
		t.Fatalf("identity trailer escaped: %v", resp.Trailer)
	}
	if resp.Trailer.Get("X-Finished") != "done" || resp.Header.Get("X-Finished") != "" {
		t.Fatalf("unrelated trailer lost or promoted: headers=%v trailers=%v", resp.Header, resp.Trailer)
	}
	if !slices.Equal(resp.Trailer.Values("X-Finished"), baseline.Trailer.Values("X-Finished")) ||
		!slices.Equal(resp.Header.Values("X-Finished"), baseline.Header.Values("X-Finished")) {
		t.Fatalf("unrelated metadata changed: got headers=%v trailers=%v; control headers=%v trailers=%v",
			resp.Header, resp.Trailer, baseline.Header, baseline.Trailer)
	}
}

func requestIDWireResponse(t *testing.T, srv *httptest.Server, path string) *http.Response {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), "GET", srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("X-Input", "current-id")
	resp, err := srv.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestRequestIDTrailerBufferedReplay(t *testing.T) {
	t.Parallel()
	for _, latePrefix := range []bool{false, true} {
		for order, mws := range [][]Middleware{
			{ETag(), RequestID().From("X-Input")},
			{Retry(2), RequestID().From("X-Input")},
			{ETag(), Retry(2), RequestID().From("X-Input")},
		} {
			t.Run(fmt.Sprintf("late=%v/order=%d", latePrefix, order), func(t *testing.T) {
				r := httptest.NewRequest("GET", "/", nil)
				r.Header.Set("X-Input", "current-id")
				rec := runRequest(t, chain(t, requestIDTrailerProducer(latePrefix), mws...), r)
				assertOnlyRequestID(t, rec.Result().Header, defaultRequestIDHeader, "current-id")
				if values, present := cacheHeaderValues(rec.Header(), http.TrailerPrefix+defaultRequestIDHeader); present {
					t.Fatalf("replayed identity trailer %v", values)
				}
			})
		}
	}
}

func TestRequestIDTrailerBufferedWire(t *testing.T) {
	t.Parallel()
	for _, h2 := range []bool{false, true} {
		for _, latePrefix := range []bool{false, true} {
			for order, mws := range [][]Middleware{{ETag()}, {Retry(2)}, {ETag(), Retry(2)}} {
				t.Run(fmt.Sprintf("h2=%v/late=%v/order=%d", h2, latePrefix, order), func(t *testing.T) {
					checkRequestIDTrailerBoundary(t, h2, latePrefix, mws)
				})
			}
		}
	}
}

type requestIDMutatingReader struct {
	h http.Header
}

func (r *requestIDMutatingReader) Read(b []byte) (int, error) {
	r.h.Set(defaultRequestIDHeader, "late-reader-id")
	return copy(b, "body"), io.EOF
}

func TestRequestIDReadFromLateHeaderMutation(t *testing.T) {
	t.Parallel()
	h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.(io.ReaderFrom).ReadFrom(&requestIDMutatingReader{h: w.Header()})
	}), ETag(), RequestID().From("X-Input"))
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Input", "current-id")
	rec := runRequest(t, h, r)
	assertOnlyRequestID(t, rec.Result().Header, defaultRequestIDHeader, "current-id")
}
