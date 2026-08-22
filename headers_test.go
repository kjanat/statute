package statute

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"statute.kjanat.dev/resolved"
)

// chain resolves the given surface middleware and wraps base with them, in
// declaration order, exactly as buildRouter does for a route.
func chain(t *testing.T, base http.Handler, mws ...Middleware) http.Handler {
	t.Helper()
	resolvedMWs := make([]resolved.Middleware, 0, len(mws))
	for _, mw := range mws {
		resolvedMWs = append(resolvedMWs, mustResolveMW(t, mw))
	}
	return wrapMiddleware(resolvedMWs, base)
}

// captureRequest returns a handler that records the request it is given.
func captureRequest(seen **http.Request) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = r
		w.WriteHeader(http.StatusOK)
	})
}

// TestResolveHeaderMW — every constructor resolves to its own discriminator
// and canonicalises the header name.
func TestResolveHeaderMW(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		mw        Middleware
		wantType  resolved.MiddlewareType
		wantName  string
		wantValue string
	}{
		{"set request", SetRequestHeader("x-forwarded-proto", "https"), resolved.MWSetRequestHeader, "X-Forwarded-Proto", "https"},
		{"add request", AddRequestHeader("x-tag", "a"), resolved.MWAddRequestHeader, "X-Tag", "a"},
		{"remove request", RemoveRequestHeader("x-forwarded-for"), resolved.MWRemoveRequestHeader, "X-Forwarded-For", ""},
		{"set response", SetResponseHeader("x-robots-tag", "noindex, nofollow"), resolved.MWSetResponseHeader, "X-Robots-Tag", "noindex, nofollow"},
		{"add response", AddResponseHeader("VARY", "Origin"), resolved.MWAddResponseHeader, "Vary", "Origin"},
		{"remove response", RemoveResponseHeader("server"), resolved.MWRemoveResponseHeader, "Server", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := mustResolveMW(t, c.mw)
			if got.Type != c.wantType {
				t.Errorf("type: got %v, want %v", got.Type, c.wantType)
			}
			if got.HeaderName != c.wantName {
				t.Errorf("name: got %q, want %q", got.HeaderName, c.wantName)
			}
			if got.HeaderValue != c.wantValue {
				t.Errorf("value: got %q, want %q", got.HeaderValue, c.wantValue)
			}
		})
	}
}

// TestResolveHeaderMWErrors — invalid names, injected values, and the
// unmutable request Host are rejected at resolve time, not at request time.
func TestResolveHeaderMWErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mw      Middleware
		wantErr string
	}{
		{"empty name", SetRequestHeader("", "v"), "header name: empty"},
		{"space in name", SetResponseHeader("X Robots", "v"), "invalid character"},
		{"colon in name", AddResponseHeader("X-Robots:", "v"), "invalid character"},
		{"newline in value", SetResponseHeader("X-Robots-Tag", "noindex\r\nX-Injected: yes"), "invalid character"},
		{"set request host", SetRequestHeader("Host", "internal.example.com"), "Host header cannot be rewritten"},
		{"add request host", AddRequestHeader("host", "internal.example.com"), "Host header cannot be rewritten"},
		{"remove request host", RemoveRequestHeader("HOST"), "Host header cannot be rewritten"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := resolveMiddleware(c.mw)
			if err == nil {
				t.Fatalf("want error containing %q", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("got %q, want substring %q", err, c.wantErr)
			}
		})
	}
}

// TestResponseHeaderHostAllowed — Host is only special on the request; a
// response may carry it like any other header.
func TestResponseHeaderHostAllowed(t *testing.T) {
	t.Parallel()
	got := mustResolveMW(t, SetResponseHeader("Host", "edge.example.com"))
	if got.HeaderName != "Host" || got.HeaderValue != "edge.example.com" {
		t.Errorf("got %q: %q", got.HeaderName, got.HeaderValue)
	}
}

// TestHeaderOpUnknownType — a discriminator that is not a header operation is
// inert: applyHeaderOp leaves the map alone rather than guessing, and the
// label falls back to the enum convention. Both branches are unreachable from
// the surface API, and stay that way only if they behave.
func TestHeaderOpUnknownType(t *testing.T) {
	t.Parallel()
	notAHeaderOp := resolved.MWCompress

	h := http.Header{"X-Existing": []string{"kept"}}
	applyHeaderOp(h, notAHeaderOp, "X-Existing", "clobbered")
	if got := h.Get("X-Existing"); got != "kept" {
		t.Errorf("X-Existing: got %q, want it untouched", got)
	}
	if len(h) != 1 {
		t.Errorf("header map grew: %v", h)
	}
	if got := headerMWLabel(notAHeaderOp); got != enumUnknown {
		t.Errorf("headerMWLabel: got %q, want %q", got, enumUnknown)
	}
}

// TestRequestHeaderMiddleware — request mutations reach the inner handler and
// apply in declaration order.
func TestRequestHeaderMiddleware(t *testing.T) {
	t.Parallel()
	var seen *http.Request
	h := chain(t, captureRequest(&seen),
		SetRequestHeader("X-Forwarded-Proto", "https"),
		RemoveRequestHeader("X-Forwarded-For"),
		AddRequestHeader("X-Tag", "first"),
		AddRequestHeader("X-Tag", "second"),
		SetRequestHeader("X-Once", "a"),
		SetRequestHeader("X-Once", "b"),
	)

	req := httptest.NewRequest("GET", "http://x/", nil)
	req.Header.Set("X-Forwarded-Proto", "http")
	req.Header.Set("X-Forwarded-For", "203.0.113.5")
	runRequest(t, h, req)

	if seen == nil {
		t.Fatal("inner handler never ran")
	}
	assertHeader(t, seen.Header, "X-Forwarded-Proto", "https")
	assertNoHeader(t, seen.Header, "X-Forwarded-For")
	if got := seen.Header.Values("X-Tag"); len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Errorf("X-Tag: got %v, want [first second]", got)
	}
	// Later declarations win: the second Set replaces the first.
	assertHeader(t, seen.Header, "X-Once", "b")
}

// TestResponseHeaderMiddleware — response mutations override what the inner
// handler produced, and apply in declaration order rather than innermost-first.
func TestResponseHeaderMiddleware(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Server", "origin/1.0")
		w.Header().Set("X-Robots-Tag", "index")
		w.WriteHeader(http.StatusOK)
	})
	h := chain(t, inner,
		SetResponseHeader("X-Robots-Tag", "noindex, nofollow"),
		RemoveResponseHeader("Server"),
		AddResponseHeader("Vary", "Origin"),
		AddResponseHeader("Vary", "Accept-Encoding"),
		// Declaration order decides: the removal runs after the set, so the
		// header is gone. Reversed, it would survive.
		SetResponseHeader("X-Temp", "value"),
		RemoveResponseHeader("X-Temp"),
	)
	rec := runRequest(t, h, httptest.NewRequest("GET", "http://x/", nil))

	assertHeader(t, rec.Header(), "X-Robots-Tag", "noindex, nofollow")
	assertNoHeader(t, rec.Header(), "Server")
	assertNoHeader(t, rec.Header(), "X-Temp")
	if got := rec.Header().Values("Vary"); len(got) != 2 {
		t.Errorf("Vary: got %v, want two values", got)
	}
}

// TestResponseHeaderMiddlewareImplicitStatus — a handler that writes a body
// without calling WriteHeader still gets the mutations.
func TestResponseHeaderMiddlewareImplicitStatus(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("body"))
	})
	h := chain(t, inner, SetResponseHeader("X-Robots-Tag", "noindex"))
	rec := runRequest(t, h, httptest.NewRequest("GET", "http://x/", nil))
	assertHeader(t, rec.Header(), "X-Robots-Tag", "noindex")
}

// TestResponseHeaderMiddlewareStreaming — the wrapper keeps a streaming
// handler streaming: a flush through http.ResponseController reaches the
// underlying writer, and commits the mutated headers with it.
func TestResponseHeaderMiddlewareStreaming(t *testing.T) {
	t.Parallel()
	var wrapped http.ResponseWriter
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		wrapped = w
		_, _ = w.Write([]byte("chunk"))
		_ = http.NewResponseController(w).Flush()
	})
	h := chain(t, inner, SetResponseHeader("X-Robots-Tag", "noindex"))
	rec := runRequest(t, h, httptest.NewRequest("GET", "http://x/", nil))

	if !rec.Flushed {
		t.Error("flush did not reach the underlying ResponseWriter")
	}
	assertHeader(t, rec.Header(), "X-Robots-Tag", "noindex")

	hw, ok := wrapped.(*headerResponseWriter)
	if !ok {
		t.Fatalf("inner handler saw %T, want *headerResponseWriter", wrapped)
	}
	if hw.Unwrap() != http.ResponseWriter(rec) {
		t.Error("Unwrap does not expose the underlying ResponseWriter")
	}
}

// TestHeaderMiddlewareThroughProxy — the mutations survive a real proxy hop:
// request headers reach the backend, and a response mutation beats the
// header the backend set.
func TestHeaderMiddlewareThroughProxy(t *testing.T) {
	t.Parallel()
	backend := newEchoBackend(t)
	r := mustResolve(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Upstreams: Upstreams{
			"api": Pool{Backends: []Backend{{Address: strings.TrimPrefix(backend.URL, "http://")}}},
		},
		Routes: Routes{
			Match("/*").ProxyTo("api").With(
				SetRequestHeader("X-Api-Version", "2"),
				SetRequestHeader("X-Forwarded-Proto", "https"),
				RemoveRequestHeader("X-Secret"),
				SetResponseHeader("X-Robots-Tag", "noindex, nofollow"),
			),
		},
	})
	srv, err := newServer(r)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	t.Cleanup(func() {
		for _, ph := range srv.pools {
			ph.shutdown()
		}
	})

	req := httptest.NewRequest("GET", "http://x/anything", nil)
	req.Header.Set("X-Secret", "leak")
	// Make the backend set the same response header, so the assertion below
	// proves the middleware overrides the origin rather than merely filling a gap.
	req.Header.Set("X-Test-Set-Header", "X-Robots-Tag: index")
	rec := runRequest(t, srv.buildRouter(), req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	echo := decodeEcho(t, rec.Body)
	if got := echo.Headers["X-Api-Version"]; len(got) != 1 || got[0] != "2" {
		t.Errorf("upstream X-Api-Version: got %v, want [2]", got)
	}
	if got, ok := echo.Headers["X-Secret"]; ok {
		t.Errorf("upstream still saw X-Secret: %v", got)
	}
	// The proxy owns the X-Forwarded-* set: ProxyRequest.SetXForwarded runs
	// after route middleware and rewrites them from the real connection, so a
	// client (or a route) cannot spoof them. Pinned so the interaction is a
	// decision rather than a surprise.
	if got := echo.Headers["X-Forwarded-Proto"]; len(got) != 1 || got[0] != "http" {
		t.Errorf("upstream X-Forwarded-Proto: got %v, want [http] from the proxy", got)
	}
	assertHeader(t, rec.Header(), "X-Robots-Tag", "noindex, nofollow")
}
