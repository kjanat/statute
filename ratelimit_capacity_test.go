package statute

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"statute.kjanat.dev/resolved"
)

func assertBucketStore(t *testing.T, s *bucketStore, want int) {
	t.Helper()
	if len(s.buckets) != want || len(s.expiry) != want || want > s.limit {
		t.Fatalf("map=%d heap=%d want=%d limit=%d", len(s.buckets), len(s.expiry), want, s.limit)
	}
	for i, b := range s.expiry {
		if b.index != i || s.buckets[b.key] != b {
			t.Fatalf("invalid bucket %d: %+v", i, b)
		}
		assertBucketTokens(t, b, s.capacity)
		if i > 0 && s.expiry.Less(i, (i-1)/2) {
			t.Fatalf("invalid heap order at %d", i)
		}
	}
}

func assertBucketTokens(t *testing.T, b *bucket, capacity float64) {
	t.Helper()
	if math.IsNaN(b.tokens) || math.IsInf(b.tokens, 0) || b.tokens < 0 || b.tokens > capacity {
		t.Fatalf("invalid bucket tokens: %+v", b)
	}
}

func assertCapacityDenied(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("capacity response: %d %v", rec.Code, rec.Header())
	}
}

func TestBucketStoreCapacityAndChurn(t *testing.T) {
	t.Parallel()
	now := time.Unix(100, 0)
	s := newBucketStore(1.0/3600, 7)
	for i := range s.limit {
		if got := s.allow(fmt.Sprint(i), now); got != 0 {
			t.Fatalf("admission %d: %d", i, got)
		}
	}
	longHost := strings.Repeat("a", 65536)
	for i := range 2000 {
		if got := s.allow(fmt.Sprint(i)+longHost, now); got != http.StatusServiceUnavailable {
			t.Fatalf("churn admitted %d: %d", i, got)
		}
		if got := s.allow("0", now.Add(30*time.Minute)); got != http.StatusTooManyRequests {
			t.Fatalf("churn reset debt: %d", got)
		}
	}
	assertBucketStore(t, s, 7)
	if got := s.allow(longHost, now.Add(time.Hour)); got != 0 {
		t.Fatalf("fully replenished slot not reclaimed: %d", got)
	}
	if s.buckets[sha256.Sum256([]byte(longHost))] == nil {
		t.Fatal("long Host not represented by fixed-size digest")
	}
	assertBucketStore(t, s, 7)
}

func TestBucketStoreExistingKeysAtCapacity(t *testing.T) {
	t.Parallel()
	now := time.Unix(100, 0)
	s := newBucketStore(1, 1)
	for _, tc := range []struct {
		key   string
		delay time.Duration
		want  int
	}{
		{"old", 0, 0},
		{"new", 0, 503},
		{"old", 0, 0},
		{"old", 0, 429},
		{"new", time.Second, 503}, // One token is not a fully replenished burst.
		{"old", time.Second, 0},
		{"new", 2 * time.Second, 503},
		{"new", 3 * time.Second, 0},
	} {
		if got := s.allow(tc.key, now.Add(tc.delay)); got != tc.want {
			t.Fatalf("key=%s delay=%s got=%d want=%d", tc.key, tc.delay, got, tc.want)
		}
		assertBucketStore(t, s, 1)
	}
}

func TestBucketStoreRetirementKeepsIndexBounded(t *testing.T) {
	t.Parallel()
	now := time.Unix(100, 0)
	s := newBucketStore(0.5, 7)
	for i := range 10000 {
		if got := s.allow(fmt.Sprint(i), now.Add(time.Duration(i)*2*time.Second)); got != 0 {
			t.Fatalf("fully replenished slot denied at %d: %d", i, got)
		}
		assertBucketStore(t, s, min(i+1, s.limit))
	}
}

func TestBucketStoreRefillEdges(t *testing.T) {
	t.Parallel()
	now := time.Unix(100, 0)
	t.Run("fractional", func(t *testing.T) {
		s := newBucketStore(0.3, 1)
		_ = s.allow("a", now)
		deadline := s.expiry[0].fullAt
		if got := s.allow("b", deadline.Add(-time.Nanosecond)); got != 503 {
			t.Fatalf("rounded deadline retired early: %d", got)
		}
		if got := s.allow("b", deadline); got != 0 {
			t.Fatalf("full fractional refill: %d", got)
		}
		assertBucketStore(t, s, 1)
	})
	t.Run("backwards", func(t *testing.T) {
		s := newBucketStore(0.5, 1)
		_ = s.allow("a", now)
		for _, at := range []time.Time{now.Add(-time.Hour), now, now.Add(time.Second)} {
			if got := s.allow("a", at); got != 429 {
				t.Fatalf("backwards clock granted tokens at %s: %d", at, got)
			}
		}
		if got := s.allow("a", now.Add(2*time.Second)); got != 0 {
			t.Fatalf("refill after clock recovery: %d", got)
		}
		assertBucketStore(t, s, 1)
	})
	t.Run("saturated", func(t *testing.T) {
		s := newBucketStore(math.SmallestNonzeroFloat64, 1)
		_ = s.allow("a", now)
		if s.expiry[0].fullAt.Sub(now) != time.Duration(math.MaxInt64) {
			t.Fatal("deadline did not saturate")
		}
		if got := s.allow("b", now.Add(time.Duration(math.MaxInt64))); got != 503 {
			t.Fatalf("saturated hint erased debt: %d", got)
		}
		assertBucketStore(t, s, 1)
	})
	t.Run("huge finite", func(t *testing.T) {
		s := newBucketStore(math.MaxFloat64, 1)
		for i := range 20 {
			if got := s.allow(fmt.Sprint(i), now.Add(time.Duration(i)*time.Second)); got != 0 {
				t.Fatalf("huge finite rate refused: %d", got)
			}
			assertBucketStore(t, s, 1)
		}
	})
}

func TestBucketStoreConcurrentAdmissionAndAccounting(t *testing.T) {
	t.Parallel()
	now := time.Unix(100, 0)
	s := newBucketStore(0.5, 31)
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for i := range 500 {
		wg.Go(func() {
			for range 10 {
				if s.allow(fmt.Sprint(i), now) == 0 {
					admitted.Add(1)
				}
			}
		})
	}
	wg.Wait()
	if admitted.Load() != 31 {
		t.Fatalf("concurrent grants=%d want=31", admitted.Load())
	}
	assertBucketStore(t, s, 31)
	for _, b := range s.expiry {
		if b.tokens != 0 {
			t.Fatal("admitted key gained extra tokens")
		}
	}
}

func TestRateLimitCapacityHTTP(t *testing.T) {
	t.Parallel()
	m, err := resolveRateLimitMW(RateLimit("1/h").Per(HostHeader).MaxBuckets(1))
	if err != nil {
		t.Fatal(err)
	}
	h := rateLimitHandler(m, noContentHandler)
	for _, tc := range []struct {
		host         string
		status       int
		retry, cache string
	}{
		{"A.example", 204, "", ""},
		{"A.example", 429, "1", ""},
		{"a.example", 503, "", "no-store"},
		{"A.example:80", 503, "", "no-store"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Host = tc.host
		w := runRequest(t, h, r)
		if w.Code != tc.status || w.Header().Get("Retry-After") != tc.retry || w.Header().Get("Cache-Control") != tc.cache {
			t.Fatalf("host=%s response=%d %v", tc.host, w.Code, w.Header())
		}
	}
}

func TestRateLimitMaxBucketsResolveAndExport(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{0, 1, 73, -1} {
		cfg := Config{
			Listeners:      Listeners{HTTP(":0")},
			Routes:         Routes{Match("/static").Handle(noContentHandler).With(RateLimit("1/h").MaxBuckets(limit))},
			FallbackRoutes: Routes{Match("/*").Handle(noContentHandler).With(RateLimit("1/h").MaxBuckets(limit))},
			Docker:         Docker().DefaultMiddleware(RateLimit("1/h").MaxBuckets(limit)).Middleware("named", RateLimit("1/h").MaxBuckets(limit)),
		}
		var out bytes.Buffer
		err := Export(cfg, &out)
		if limit < 0 {
			if err == nil || !strings.Contains(err.Error(), "max buckets") {
				t.Fatalf("negative cap: %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		var got resolved.Config
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		want := limit
		if want == 0 {
			want = defaultRateLimitMaxBuckets
		}
		for _, m := range []resolved.Middleware{got.Routes[0].Middleware[0], got.FallbackRoutes[0].Middleware[0], got.Docker.DefaultMiddleware[0], got.Docker.Middleware["named"][0]} {
			if m.RateLimitMaxBuckets != want {
				t.Fatalf("cap=%d want=%d", m.RateLimitMaxBuckets, want)
			}
		}
	}
}

func TestRateLimitCapacityCachePlacement(t *testing.T) {
	t.Parallel()
	for _, outside := range []bool{true, false} {
		mws := []Middleware{Cache("1h"), RateLimit("1/h").MaxBuckets(1)}
		if outside {
			mws[0], mws[1] = mws[1], mws[0]
		}
		mw, err := resolveMiddlewares(mws)
		if err != nil {
			t.Fatal(err)
		}
		calls := 0
		h := wrapMiddleware(mw, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			w.WriteHeader(http.StatusOK)
		}))
		request := func(path, peer string) *httptest.ResponseRecorder {
			r := httptest.NewRequest("GET", path, nil)
			r.RemoteAddr = peer + ":1234"
			return runRequest(t, h, r)
		}
		if rec := request("/cached", "192.0.2.1"); rec.Code != 200 {
			t.Fatal(rec.Code)
		}
		want := 200
		if outside {
			want = 503
		}
		if rec := request("/cached", "192.0.2.2"); rec.Code != want {
			t.Fatalf("outside=%v got=%d want=%d", outside, rec.Code, want)
		}
		assertCapacityDenied(t, request("/miss", "192.0.2.2"))
		if rec := request("/miss", "192.0.2.1"); rec.Code != 429 {
			t.Fatalf("capacity denial cached: %d", rec.Code)
		}
		if calls != 1 {
			t.Fatalf("producer calls=%d", calls)
		}
	}
}

func TestRateLimitCapacityRetryPlacement(t *testing.T) {
	t.Parallel()
	for _, outside := range []bool{true, false} {
		calls := 0
		producer := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(502) })
		mws := []Middleware{Retry(3, OnStatus(502, 503, 429)), RateLimit("1/h").MaxBuckets(1)}
		if outside {
			mws[0], mws[1] = mws[1], mws[0]
		}
		mw, err := resolveMiddlewares(mws)
		if err != nil {
			t.Fatal(err)
		}
		h := wrapMiddleware(mw, producer)
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = "192.0.2.1:1234"
		wantStatus, wantCalls := 429, 1
		if outside {
			wantStatus, wantCalls = 502, 3
		}
		if rec := runRequest(t, h, r); rec.Code != wantStatus || calls != wantCalls {
			t.Fatalf("outside=%v status=%d calls=%d", outside, rec.Code, calls)
		}
		r.RemoteAddr = "192.0.2.2:1234"
		if rec := runRequest(t, h, r); rec.Code != 503 || rec.Header().Get("Cache-Control") != "no-store" || calls != wantCalls {
			t.Fatalf("retry capacity response=%d %v calls=%d", rec.Code, rec.Header(), calls)
		}
	}
}

func TestRateLimitCapacityRetryStatusPolicy(t *testing.T) {
	t.Parallel()
	// Count actual outer Retry invocations, including capacity denials.
	for _, onStatus := range []int{502, 503} {
		limit, err := resolveRateLimitMW(RateLimit("1/h").MaxBuckets(1))
		if err != nil {
			t.Fatal(err)
		}
		limiter := rateLimitHandler(limit, noContentHandler)
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = "192.0.2.1:1"
		runRequest(t, limiter, r)
		attempts := 0
		retry, err := resolveRetryMW(Retry(3, OnStatus(onStatus)))
		if err != nil {
			t.Fatal(err)
		}
		h := retryHandler(retry, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { attempts++; limiter.ServeHTTP(w, r) }))
		r.RemoteAddr = "192.0.2.2:1"
		rec := runRequest(t, h, r)
		want := 1
		if onStatus == 503 {
			want = 3
		}
		if rec.Code != 503 || attempts != want || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("status=%d attempts=%d want=%d headers=%v", rec.Code, attempts, want, rec.Header())
		}
	}
}

func TestRateLimitCapacityRouteAndFallbackIsolation(t *testing.T) {
	t.Parallel()
	backend := httptest.NewServer(noContentHandler)
	t.Cleanup(backend.Close)
	policy := RateLimit("1/h").MaxBuckets(1)
	h := fallbackRouter(t, Config{
		Listeners:      Listeners{HTTP(":0")},
		Upstreams:      Upstreams{"shared": Pool{Backends: []Backend{{Address: strings.TrimPrefix(backend.URL, "http://")}}}},
		Routes:         Routes{Match("/*").Hosts("a.example", "b.example").ProxyTo("shared").With(policy)},
		FallbackRoutes: Routes{Match("/*").ProxyTo("shared").With(policy)},
	})
	for _, host := range []string{"a.example", "b.example", "fallback.example"} {
		for _, tc := range []struct {
			peer   string
			status int
		}{{"192.0.2.1", 204}, {"192.0.2.2", 503}, {"192.0.2.1", 429}} {
			r := httptest.NewRequest("GET", "http://"+host+"/", nil)
			r.RemoteAddr = tc.peer + ":1"
			if rec := runRequest(t, h, r); rec.Code != tc.status {
				t.Fatalf("host=%s peer=%s got=%d want=%d", host, tc.peer, rec.Code, tc.status)
			}
		}
	}
}

func TestRateLimitCapacityEffectiveClientIP(t *testing.T) {
	t.Parallel()
	for _, trusted := range []bool{false, true} {
		m, err := resolveRateLimitMW(RateLimit("1/h").MaxBuckets(1))
		if err != nil {
			t.Fatal(err)
		}
		h := rateLimitHandler(m, noContentHandler)
		if trusted {
			h = trustedProxyMiddleware(trustedListener(t, TrustedProxy("192.0.2.0/24")), h)
		}
		for i, forwarded := range []string{"198.51.100.1", "198.51.100.2"} {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = "192.0.2.1:1234"
			r.Header.Set("X-Forwarded-For", forwarded)
			want := 204
			if i > 0 {
				want = 429
				if trusted {
					want = 503
				}
			}
			if rec := runRequest(t, h, r); rec.Code != want {
				t.Fatalf("trusted=%v request=%d got=%d want=%d", trusted, i, rec.Code, want)
			}
		}
	}
}

func TestRateLimitCapacityDockerGenerationIsolation(t *testing.T) {
	t.Parallel()
	backend := httptest.NewServer(noContentHandler)
	t.Cleanup(backend.Close)
	host, port := backendHostPort(t, backend)
	cfg, err := resolveDocker(Docker().TraefikLabels().Middleware("limited", RateLimit("1/h").MaxBuckets(1)))
	if err != nil {
		t.Fatal(err)
	}
	p, srv, _ := newFakeProvider(t, cfg, []fakeDaemonContainer{{name: "app", ip: host, port: port, labels: map[string]string{
		"traefik.enable":                     "true",
		"traefik.http.routers.a.rule":        "Host(`a.example`)",
		"traefik.http.routers.a.service":     "shared",
		"traefik.http.routers.a.middlewares": "limited",
		"traefik.http.routers.b.rule":        "Host(`b.example`)",
		"traefik.http.routers.b.service":     "shared",
		"traefik.http.routers.b.middlewares": "limited",
	}}})
	mustSync(t, p)
	old := srv.dynamic.Load()
	request := func(tab *dynamicTable, host, peer string) int {
		r := httptest.NewRequest("GET", "http://"+host+"/", nil)
		r.RemoteAddr = peer + ":1"
		h := findHandler(tab.routes, host, r)
		if h == nil {
			t.Fatal("missing Docker handler")
		}
		return runRequest(t, h, r).Code
	}
	for _, host := range []string{"a.example", "b.example"} {
		if got := request(old, host, "192.0.2.1"); got != 204 {
			t.Fatalf("%s first=%d", host, got)
		}
		if got := request(old, host, "192.0.2.2"); got != 503 {
			t.Fatalf("%s capacity=%d", host, got)
		}
	}
	mustSync(t, p)
	fresh := srv.dynamic.Load()
	if fresh == old {
		t.Fatal("generation was not replaced")
	}
	if got := request(fresh, "a.example", "192.0.2.2"); got != 204 {
		t.Fatalf("new generation inherited buckets: %d", got)
	}
	if got := request(old, "a.example", "192.0.2.2"); got != 503 {
		t.Fatalf("old generation mutated: %d", got)
	}
}
