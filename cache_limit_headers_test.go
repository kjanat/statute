package statute

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/propagation"
)

func TestCacheOversizedInformationalHeaders(t *testing.T) {
	h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", strings.Repeat("x", 65<<10))
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Del("Link")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "unavailable")
	}), interactionCache("16B"))
	rec := runRequest(t, h, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != "unavailable" {
		t.Fatalf("final status=%d body=%q", rec.Code, rec.Body.String())
	}
}

type cacheUnknownLengthTransport struct{ calls int }

func (t *cacheUnknownLengthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.calls++
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
		Body:          io.NopCloser(strings.NewReader("<html>representation</html>")),
		ContentLength: -1, Request: r}, nil
}

func TestCacheUnknownLengthProxyFlush(t *testing.T) {
	for _, tc := range []struct {
		limit string
		calls int
	}{{"64B", 1}, {"8B", 2}} {
		t.Run(tc.limit, func(t *testing.T) {
			transport := &cacheUnknownLengthTransport{}
			proxy := &httputil.ReverseProxy{Transport: transport, Rewrite: func(r *httputil.ProxyRequest) {
				r.Out.URL.Scheme, r.Out.URL.Host = "http", "origin.example"
			}}
			h := chain(t, proxy, interactionCache(tc.limit))
			for range 2 {
				rec := runRequest(t, h, httptest.NewRequest(http.MethodGet, "/", nil))
				if rec.Code != http.StatusOK || rec.Body.String() != "<html>representation</html>" {
					t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
				}
			}
			if transport.calls != tc.calls {
				t.Fatalf("origin calls=%d want=%d", transport.calls, tc.calls)
			}
		})
	}
}

func TestCacheOverflowContentTypeOnWire(t *testing.T) {
	for _, mode := range []string{"first-write", "prefix", "headers"} {
		t.Run(mode, func(t *testing.T) {
			h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if mode == "headers" {
					w.Header().Set("X-Large", strings.Repeat("x", 65<<10))
					w.WriteHeader(http.StatusOK)
				}
				if mode == "prefix" {
					_, _ = io.WriteString(w, "  ")
				}
				_, _ = io.WriteString(w, "<html><body>representation</body></html>")
			}), interactionCache("16B"))
			srv := httptest.NewServer(h)
			defer srv.Close()
			resp, err := srv.Client().Get(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if got := resp.Header.Get("Content-Type"); got != "text/html; charset=utf-8" {
				t.Fatalf("Content-Type=%q", got)
			}
		})
	}
}

func TestCachePropagationSnapshotBounds(t *testing.T) {
	for _, fields := range [][]string{make([]string, 257), {strings.Repeat("x", 16<<10)}} {
		p := cacheTestPropagator{fields: func() []string { return fields }, inject: func(_ context.Context, c propagation.TextMapCarrier) { c.Set("X-Injected", "yes") }}
		names, signature, valid := cachePropagationFields(p)
		if valid || names != nil || signature != "" {
			t.Fatalf("oversized declaration copied: fields=%d signature=%d valid=%t", len(names), len(signature), valid)
		}
		policy := &cacheProxyPolicy{propagator: p}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r = r.WithContext(context.WithValue(r.Context(), cacheProxyPolicyKey{}, policy))
		injectProxyPropagation(r)
		if !policy.unsafe.Load() || r.Header.Get("X-Injected") != "yes" {
			t.Fatal("oversized declaration changed injection or remained cacheable")
		}
	}
}
