package statute

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

type cacheIdentityCase struct {
	name, identity, policy, vary string
	cookie                       bool
}

func (tc cacheIdentityCase) serve(w http.ResponseWriter, r *http.Request) {
	var identity string
	switch r.Header.Get(tc.identity) {
	case "victim":
		identity = "victim"
	case "other":
		identity = "other"
	default:
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	if tc.policy != "" {
		w.Header().Set("Cache-Control", tc.policy)
	}
	if tc.vary != "" {
		w.Header().Set("Vary", tc.vary)
	}
	if tc.cookie {
		w.Header().Set("Set-Cookie", "session="+identity+"; HttpOnly; Secure")
	}
	_, _ = io.WriteString(w, identity+" account secret")
}

func (tc cacheIdentityCase) expectedCookie(identity string) string {
	if tc.cookie && identity != "" {
		return "session=" + identity + "; HttpOnly; Secure"
	}
	return ""
}

func assertCacheIdentity(t *testing.T, srv *httptest.Server, tc cacheIdentityCase, identity string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/account?tab=profile", nil)
	if err != nil {
		t.Fatal(err)
	}
	if identity != "" {
		req.Header.Set(tc.identity, identity)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	wantStatus, wantBody := http.StatusOK, identity+" account secret"
	if identity == "" {
		wantStatus, wantBody = http.StatusUnauthorized, "authentication required\n"
	}
	if resp.StatusCode != wantStatus || string(body) != wantBody {
		t.Fatalf("client %q: status=%d body=%q; want %d %q", identity, resp.StatusCode, body, wantStatus, wantBody)
	}
	wantCookie := tc.expectedCookie(identity)
	if got := resp.Header.Get("Set-Cookie"); got != wantCookie {
		t.Fatalf("client %q received cookie %q, want %q", identity, got, wantCookie)
	}
}

func TestCacheCrossClientIsolation(t *testing.T) {
	t.Parallel()
	for _, tc := range []cacheIdentityCase{
		{"reported no-store and vary", "Authorization", "private, no-store", "Authorization, Cookie", true},
		{"authorization", "Authorization", "", "", false},
		{"cookie", "Cookie", "", "", false},
		{"private", "X-Identity", "private", "", false},
		{"qualified private", "X-Identity", `private="Set-Cookie"`, "", true},
		{"cookie issuance", "X-Identity", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				tc.serve(w, r)
			}), Cache("1h"))
			srv := httptest.NewServer(h)
			defer srv.Close()
			for _, identity := range []string{"victim", "", "other"} {
				assertCacheIdentity(t, srv, tc, identity)
			}
			if calls.Load() != 3 {
				t.Fatalf("downstream calls=%d, want 3", calls.Load())
			}
		})
	}
}

func TestCacheCredentialsBypassWarmPublicEntry(t *testing.T) {
	t.Parallel()
	for _, method := range []string{"GET", "HEAD"} {
		for _, name := range []string{"Authorization", "authorization", "Cookie", "cOoKiE"} {
			t.Run(method+name, func(t *testing.T) {
				t.Parallel()
				calls := 0
				h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls++
					w.Header().Set("Cache-Control", "public, s-maxage=3600")
					w.Header().Set("X-Call", fmt.Sprint(calls))
				}), Cache("1h"))
				for i, values := range [][]string{nil, {"secret"}, {""}, {}, nil} {
					req := httptest.NewRequest(method, "/", nil)
					if i > 0 && i < 4 {
						req.Header[name] = values
					}
					rec := runRequest(t, h, req)
					want := i + 1
					if i == 4 {
						want = 1
					}
					if rec.Header().Get("X-Call") != fmt.Sprint(want) {
						t.Fatalf("request %d served %v", i, rec.Header())
					}
				}
				if calls != 4 {
					t.Fatalf("calls=%d, want 4", calls)
				}
			})
		}
	}
}

func TestCachePrivacyHeaderProjection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, header string
		values       []string
		operations   []Middleware
	}{
		{"private removed", "Cache-Control", []string{"private"}, []Middleware{RemoveResponseHeader("Cache-Control")}},
		{"private replaced", "cache-control", []string{"public", `PrIvAtE="X-Secret"`}, []Middleware{SetResponseHeader("Cache-Control", "public")}},
		{"cookie removed", "Set-Cookie", []string{"session=secret"}, []Middleware{RemoveResponseHeader("Set-Cookie")}},
		{"empty cookie", "sEt-CoOkIe", []string{""}, nil},
		{"route private", "", nil, []Middleware{AddResponseHeader("Cache-Control", "private")}},
		{"route cookie", "", nil, []Middleware{SetResponseHeader("Set-Cookie", "session=secret")}},
	} {
		for _, outer := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/retryOutside=%v", tc.name, outer), func(t *testing.T) {
				t.Parallel()
				calls := 0
				mws := []Middleware{Cache("1h"), Retry(2, OnStatus(503))}
				if outer {
					mws[0], mws[1] = mws[1], mws[0]
				}
				mws = append(mws, tc.operations...)
				h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls++
					if calls%2 == 1 {
						w.WriteHeader(503)
						return
					}
					if tc.header != "" {
						w.Header()[tc.header] = tc.values
					}
				}), mws...)
				for range 2 {
					if rec := runRequest(t, h, httptest.NewRequest("GET", "/", nil)); rec.Code != 200 {
						t.Fatal(rec.Code)
					}
				}
				if calls != 4 {
					t.Fatalf("private response reused: calls=%d", calls)
				}
			})
		}
	}
}

func TestCacheCannotBypassBasicAuth(t *testing.T) {
	t.Parallel()
	hash, err := bcrypt.GenerateFromPassword([]byte("password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	for _, outer := range []bool{false, true} {
		t.Run(fmt.Sprint(outer), func(t *testing.T) {
			t.Parallel()
			calls := 0
			mws := []Middleware{Cache("1h"), BasicAuth("test", map[string]string{"victim": string(hash)})}
			if outer {
				mws[0], mws[1] = mws[1], mws[0]
			}
			h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; _, _ = io.WriteString(w, "secret") }), mws...)
			for _, password := range []string{"password", "", "wrong", "password"} {
				req := httptest.NewRequest("GET", "/", nil)
				want := http.StatusUnauthorized
				if password != "" {
					req.SetBasicAuth("victim", password)
				}
				if password == "password" {
					want = http.StatusOK
				}
				rec := runRequest(t, h, req)
				if rec.Code != want {
					t.Fatalf("password=%q: status=%d body=%q", password, rec.Code, rec.Body.String())
				}
			}
			if calls != 2 {
				t.Fatalf("authenticated responses cached: calls=%d", calls)
			}
		})
	}
}

func TestCacheInnerCredentialWriter(t *testing.T) {
	t.Parallel()
	for _, etag := range []bool{false, true} {
		t.Run(fmt.Sprint(etag), func(t *testing.T) {
			t.Parallel()
			mws := []Middleware{Cache("1h")}
			if etag {
				mws = append(mws, ETag())
			}
			mws = append(mws, RequestID().Header("Authorization").From("X-Credential"), BasicAuth("test", map[string]string{"alice": hunter2Hash}))
			h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "secret") }), mws...)
			for _, valid := range []bool{true, false} {
				req := httptest.NewRequest("GET", "/", nil)
				want := http.StatusUnauthorized
				if valid {
					req.SetBasicAuth("alice", "hunter2")
					req.Header.Set("X-Credential", req.Header.Get("Authorization"))
					req.Header.Del("Authorization")
					want = http.StatusOK
				}
				rec := runRequest(t, h, req)
				if rec.Code != want {
					t.Fatalf("valid=%v: status=%d body=%q", valid, rec.Code, rec.Body.String())
				}
			}
		})
	}
}

func TestCacheCredentialWriterRouteIsolation(t *testing.T) {
	t.Parallel()
	for _, header := range []string{"authorization", "cOoKiE"} {
		t.Run(header, func(t *testing.T) {
			t.Parallel()
			calls := 0
			base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.Header().Set("X-Call", fmt.Sprint(calls))
			})
			private := chain(t, base, Cache("1h"), ETag(), Retry(2, OnStatus(503)), RequestID().Header(header).From("X-Credential"), Cache("1h"))
			public := chain(t, base, Cache("1h"), RequestID())
			for i, h := range []http.Handler{public, private, public, private, public} {
				rec := runRequest(t, h, httptest.NewRequest("GET", "/", nil))
				if i%2 == 0 && rec.Header().Get("X-Call") != "1" {
					t.Fatalf("public sibling lost cache: %v", rec.Header())
				}
			}
			if calls != 3 {
				t.Fatalf("credential writer cached or policy leaked: calls=%d", calls)
			}
		})
	}
}
