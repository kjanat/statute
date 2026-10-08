package statute

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

type cacheTestPropagator struct {
	fields func() []string
	inject func(context.Context, propagation.TextMapCarrier)
}

func (p cacheTestPropagator) Fields() []string { return p.fields() }
func (p cacheTestPropagator) Extract(ctx context.Context, _ propagation.TextMapCarrier) context.Context {
	return ctx
}
func (p cacheTestPropagator) Inject(ctx context.Context, c propagation.TextMapCarrier) {
	p.inject(ctx, c)
}

type cacheTestIdentityKey struct{}

func cacheIdentityRequest(identity string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	return r.WithContext(context.WithValue(r.Context(), cacheTestIdentityKey{}, identity))
}

func TestCacheCustomContextIdentity(t *testing.T) {
	t.Parallel()
	for _, policy := range []string{"uncached", "private", "no-store", "unprotected cached control"} {
		t.Run(policy, func(t *testing.T) {
			calls := 0
			var mws []Middleware
			if policy != "uncached" {
				mws = []Middleware{Cache("1h")}
			}
			h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if policy == "private" || policy == "no-store" {
					w.Header().Set("Cache-Control", policy)
				}
				w.Header().Set("X-Selected", r.Context().Value(cacheTestIdentityKey{}).(string))
			}), mws...)
			for _, identity := range []string{"alice", "bob"} {
				want := identity
				if policy == "unprotected cached control" {
					want = "alice"
				}
				if rec := runRequest(t, h, cacheIdentityRequest(identity)); rec.Header().Get("X-Selected") != want {
					t.Fatalf("policy=%s: %v want %s", policy, rec.Header(), want)
				}
			}
			wantCalls := 2
			if policy == "unprotected cached control" {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("calls=%d want=%d", calls, wantCalls)
			}
		})
	}
}

func TestCachePropagationFieldNormalization(t *testing.T) {
	t.Parallel()
	p := cacheTestPropagator{fields: func() []string { return []string{"X-B", "x-a", "X-A"} }}
	fields, signature, valid := cachePropagationFields(p)
	if !valid || fmt.Sprint(fields) != "[x-a x-b]" || signature != "x-a\x00x-b" {
		t.Fatalf("fields=%v signature=%q valid=%t", fields, signature, valid)
	}
	for _, name := range []string{"", "bad:field", "X-A\x00X-B", "X A"} {
		p.fields = func() []string { return []string{name} }
		if _, _, valid = cachePropagationFields(p); valid {
			t.Fatalf("invalid field accepted: %q", name)
		}
	}
}

// Global registration has one-time delegation semantics. A separate process
// keeps those semantics intact and isolates registration from other test suites.
func TestCacheNativePropagation(t *testing.T) {
	if os.Getenv("STATUTE_TEST_CACHE_PROPAGATION") != "1" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestCacheNativePropagation$", "-test.v", "-test.timeout=60s") //nolint:gosec // G204: os.Executable identifies this test binary; arguments are fixed.
		cmd.Env = append(os.Environ(), "STATUTE_TEST_CACHE_PROPAGATION=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("isolated propagation tests: %v\n%s", err, out)
		}
		return
	}

	t.Run("initial delegate changes before Inject", testCacheInitialDelegator)
	t.Run("injected fields", testCacheInjectedFields)
	t.Run("stable nonvarying hits do not inject", testCacheStablePropagation)
	t.Run("uncached routes", testCacheUncachedPropagation)
	t.Run("old in-flight render cannot poison replacement", testCacheReplacedPropagation)
	t.Run("drift across wrappers", testCachePropagationDrift)
}

func testCacheInitialDelegator(t *testing.T) {
	// Snapshot the real initial delegator before its first registration.
	captured := newCacheProxyPolicy()
	otel.SetTextMapPropagator(cacheTestPropagator{
		fields: func() []string { return []string{"Authorization"} },
		inject: func(_ context.Context, c propagation.TextMapCarrier) { c.Set("Authorization", "secret") },
	})
	r := httptest.NewRequest("GET", "/", nil)
	r = r.WithContext(context.WithValue(r.Context(), cacheProxyPolicyKey{}, captured))
	injectProxyPropagation(r)
	if r.Header.Get("Authorization") != "secret" || !captured.unsafe.Load() || captured.usable() {
		t.Fatal("initial delegated field change was not latched")
	}
}

func testCacheInjectedFields(t *testing.T) {
	for _, field := range []string{"Traceparent", "Tracestate", "Baggage", "X-Identity", "Authorization", "Cookie", "Cache-Control", "Pragma", "If-None-Match", "Range"} {
		t.Run(field, func(t *testing.T) {
			var injections, calls atomic.Int32
			otel.SetTextMapPropagator(cacheTestPropagator{
				fields: func() []string { return []string{field} },
				inject: func(ctx context.Context, c propagation.TextMapCarrier) {
					injections.Add(1)
					c.Set(field, ctx.Value(cacheTestIdentityKey{}).(string))
				},
			})
			h := cacheNativeRouter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				// Control fields must bypass even when the origin omits Vary.
				switch field {
				case "Traceparent", "Tracestate", "Baggage", "X-Identity":
					w.Header().Set("Vary", field)
				}
				w.Header().Set("X-Selected", r.Header.Get(field))
			}), Cache("1h"))
			for _, identity := range []string{"alice", "bob", "alice"} {
				if rec := runRequest(t, h, cacheIdentityRequest(identity)); rec.Code != 200 || rec.Header().Get("X-Selected") != identity {
					t.Fatalf("%s: %d %v", identity, rec.Code, rec.Header())
				}
			}
			if calls.Load() != 3 || injections.Load() != 3 {
				t.Fatalf("calls=%d injections=%d", calls.Load(), injections.Load())
			}
		})
	}

}

func testCacheStablePropagation(t *testing.T) {
	var injections atomic.Int32
	otel.SetTextMapPropagator(cacheTestPropagator{
		fields: func() []string { return []string{"X-Trace"} },
		inject: func(_ context.Context, c propagation.TextMapCarrier) { injections.Add(1); c.Set("X-Trace", "trace") },
	})
	h := cacheNativeRouter(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Header().Set("X-Selected", "public") }), Cache("1h"))
	for range 3 {
		if rec := runRequest(t, h, cacheIdentityRequest("unused")); rec.Code != 200 {
			t.Fatal(rec.Code)
		}
	}
	if injections.Load() != 1 {
		t.Fatalf("injections=%d want 1", injections.Load())
	}
}

func testCacheUncachedPropagation(t *testing.T) {
	for _, tc := range []struct {
		name string
		mws  []Middleware
	}{
		{"no cache", nil},
		{"disabled cache", []Middleware{Cache("0s")}},
		{"credential writer disables cache", []Middleware{RequestID().Header("Authorization"), Cache("1h")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var fields, injections atomic.Int32
			otel.SetTextMapPropagator(cacheTestPropagator{
				fields: func() []string { fields.Add(1); return nil },
				inject: func(context.Context, propagation.TextMapCarrier) { injections.Add(1) },
			})
			h := cacheNativeRouter(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), tc.mws...)
			for range 2 {
				if rec := runRequest(t, h, cacheIdentityRequest("unused")); rec.Code != 200 {
					t.Fatal(rec.Code)
				}
			}
			if fields.Load() != 0 || injections.Load() != 2 {
				t.Fatalf("fields=%d injections=%d", fields.Load(), injections.Load())
			}
		})
	}

}

func testCacheReplacedPropagation(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var oldCalls atomic.Int32
	otel.SetTextMapPropagator(cacheTestPropagator{
		fields: func() []string { return []string{"X-Old"} },
		inject: func(_ context.Context, c propagation.TextMapCarrier) { c.Set("X-Old", "yes") },
	})
	h := cacheNativeRouter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		selected := "new"
		if r.Header.Get("X-Old") != "" {
			selected = "old"
			if oldCalls.Add(1) == 1 {
				close(started)
				<-release
			}
		}
		w.Header().Set("X-Selected", selected)
	}), Cache("1h"))
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { rec := httptest.NewRecorder(); h.ServeHTTP(rec, cacheIdentityRequest("unused")); done <- rec }()
	<-started
	otel.SetTextMapPropagator(cacheTestPropagator{
		fields: func() []string { return []string{"X-New"} },
		inject: func(_ context.Context, c propagation.TextMapCarrier) { c.Set("X-New", "yes") },
	})
	firstNew := runRequest(t, h, cacheIdentityRequest("unused"))
	close(release)
	old := <-done
	againNew := runRequest(t, h, cacheIdentityRequest("unused"))
	if old.Header().Get("X-Selected") != "old" || firstNew.Header().Get("X-Selected") != "new" || againNew.Header().Get("X-Selected") != "new" {
		t.Fatalf("old=%v first=%v again=%v", old.Header(), firstNew.Header(), againNew.Header())
	}
}

func testCachePropagationDrift(t *testing.T) {
	for _, mws := range [][]Middleware{
		{Cache("1h"), Retry(2, OnStatus(200)), Cache("1h")},
		{ETag(), Cache("1h"), Retry(2, OnStatus(200)), Cache("1h")},
	} {
		t.Run(fmt.Sprintf("drift across %d wrappers", len(mws)), func(t *testing.T) {
			var changed atomic.Bool
			var calls, injections atomic.Int32
			p := cacheTestPropagator{
				fields: func() []string {
					if changed.Load() {
						return []string{"Authorization"}
					}
					return nil
				},
				inject: func(ctx context.Context, c propagation.TextMapCarrier) {
					injections.Add(1)
					changed.Store(true)
					c.Set("Authorization", ctx.Value(cacheTestIdentityKey{}).(string))
				},
			}
			otel.SetTextMapPropagator(p)
			h := cacheNativeRouter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("X-Selected", r.Header.Get("Authorization"))
			}), mws...)
			for _, identity := range []string{"alice", "bob"} {
				// Both external requests start in the old empty-field namespace.
				changed.Store(false)
				if rec := runRequest(t, h, cacheIdentityRequest(identity)); rec.Header().Get("X-Selected") != identity {
					t.Fatalf("identity=%s response=%v", identity, rec.Header())
				}
			}
			if calls.Load() != 4 || injections.Load() != 4 {
				t.Fatalf("calls=%d injections=%d, want 4", calls.Load(), injections.Load())
			}
		})
	}
}
