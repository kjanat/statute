package statute

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func cacheNativeRouter(t *testing.T, origin http.Handler, mws ...Middleware) http.Handler {
	t.Helper()
	backend := httptest.NewServer(origin)
	t.Cleanup(backend.Close)
	return fallbackRouter(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Upstreams: Upstreams{"origin": Pool{Backends: []Backend{{Address: backend.URL}}}},
		Routes:    Routes{Match("/*").ProxyTo("origin").With(mws...)},
	})
}

func TestCacheNativeForwardedIdentity(t *testing.T) {
	for _, field := range []string{"X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host", "Forwarded"} {
		for _, projected := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/projected=%t", field, projected), func(t *testing.T) {
				var calls atomic.Int32
				mws := []Middleware{Cache("1h")}
				if projected {
					mws = append(mws, SetResponseHeader("Vary", field))
				} else {
					mws = append(mws, RemoveResponseHeader("Vary"))
				}
				h := cacheNativeRouter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if !projected {
						w.Header().Set("Vary", field)
					}
					w.Header().Set("X-Selected", r.Header.Get(field))
				}), mws...)
				for i, ip := range []string{"192.0.2.1", "192.0.2.2", "192.0.2.1"} {
					r := httptest.NewRequest("GET", "/", nil)
					r.RemoteAddr = ip + ":1234"
					if i == 1 {
						r.TLS = &tls.ConnectionState{}
					}
					rec := runRequest(t, h, r)
					want := expectedCacheForwardedField(r, field, ip)
					if rec.Code != 200 || rec.Header().Get("X-Selected") != want {
						t.Fatalf("request %d: %d %v, want %q", i, rec.Code, rec.Header(), want)
					}
				}
				if calls.Load() != 3 {
					t.Fatalf("late-bound Vary stored: calls=%d", calls.Load())
				}
			})
		}
	}
}

func expectedCacheForwardedField(r *http.Request, field, ip string) string {
	switch field {
	case "X-Forwarded-For":
		return ip
	case "X-Forwarded-Proto":
		if r.TLS != nil {
			return "https"
		}
		return "http"
	case "X-Forwarded-Host":
		return r.Host
	default:
		return ""
	}
}

func TestCacheNativeConnectionNomination(t *testing.T) {
	for _, nominatedFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(nominatedFirst), func(t *testing.T) {
			var calls atomic.Int32
			h := cacheNativeRouter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Vary", "X-Variant")
				w.Header().Set("X-Selected", r.Header.Get("X-Variant"))
			}), Cache("1h"))
			for _, nominated := range []bool{nominatedFirst, !nominatedFirst, nominatedFirst, !nominatedFirst} {
				r := httptest.NewRequest("GET", "/", nil)
				r.Header.Set("X-Variant", "alpha")
				want := "alpha"
				if nominated {
					r.Header.Set("Connection", "X-Variant")
					want = ""
				}
				if rec := runRequest(t, h, r); rec.Code != 200 || rec.Header().Get("X-Selected") != want {
					t.Fatalf("nominated=%t: %d %v, want %q", nominated, rec.Code, rec.Header(), want)
				}
			}
			if calls.Load() != 3 {
				t.Fatalf("calls=%d, want 3", calls.Load())
			}
		})
	}
}

func TestCacheNativeConnectionPresence(t *testing.T) {
	for _, name := range []string{"Connection", "connection", "cOnNeCtIoN"} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			h := cacheNativeRouter(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-Call", fmt.Sprint(calls.Add(1)))
			}), Cache("1h"))
			for _, values := range [][]string{nil, {""}, {"keep-alive"}, {"close"}} {
				for range 2 {
					r := httptest.NewRequest("GET", "/", nil)
					r.Header[name] = values
					if rec := runRequest(t, h, r); rec.Code != 200 {
						t.Fatal(rec.Code)
					}
				}
			}
			if calls.Load() != 8 {
				t.Fatalf("Connection presence did not bypass: %d", calls.Load())
			}
		})
	}
}

func TestCacheOriginalTargetAndScheme(t *testing.T) {
	t.Parallel()
	for _, rewrite := range []Middleware{ReplacePath("/same"), RewritePath("^/[^?]*", "/same")} {
		calls := 0
		h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.Header().Set("X-Selected", fmt.Sprintf("%s|%s|%s|%t", r.RequestURI, r.URL.RequestURI(), r.URL.Scheme, r.TLS != nil))
		}), Cache("1h"), rewrite)
		for range 2 {
			for _, original := range []string{"/one", "/two", "/a%2Fb", "/one?a=1", "/one?a=2"} {
				for _, scheme := range []string{"", "http", "https"} {
					for _, secure := range []bool{false, true} {
						assertCacheTargetIdentity(t, h, original, scheme, secure)
					}
				}
			}
		}
		if calls != 30 {
			t.Fatalf("calls=%d, want 30 distinct identities", calls)
		}
	}
}

func assertCacheTargetIdentity(t *testing.T, h http.Handler, original, scheme string, secure bool) {
	t.Helper()
	r := httptest.NewRequest("GET", original, nil)
	r.URL.Scheme = scheme
	if secure {
		r.TLS = &tls.ConnectionState{}
	}
	before := *r.URL
	rec := runRequest(t, h, r)
	effective := "/same"
	if r.URL.RawQuery != "" {
		effective += "?" + r.URL.RawQuery
	}
	want := fmt.Sprintf("%s|%s|%s|%t", original, effective, scheme, secure)
	if rec.Header().Get("X-Selected") != want {
		t.Fatalf("%v: got %q want %q", r.URL, rec.Header().Get("X-Selected"), want)
	}
	if *r.URL != before || r.RequestURI != original {
		t.Fatal("request mutated")
	}
}
