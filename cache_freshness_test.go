package statute

import (
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"statute.kjanat.dev/resolved"
)

func TestCacheFreshnessLifetime(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		header http.Header
		left   time.Duration
		valid  bool
	}{
		{"legacy TTL", nil, time.Hour, true},
		{"max age", http.Header{"Cache-Control": {"max-age=60"}}, time.Minute, true},
		{"TTL ceiling", http.Header{"Cache-Control": {"max-age=7200"}}, time.Hour, true},
		{"shared precedence", http.Header{"Cache-Control": {"max-age=1, s-maxage=90"}}, 90 * time.Second, true},
		{"shared zero", http.Header{"Cache-Control": {"max-age=90, s-maxage=0"}}, 0, true},
		{"quoted and case", http.Header{"cache-control": {`S-MaXaGe = "90"`}}, 90 * time.Second, true},
		{"extension commas", http.Header{"Cache-Control": {`foo="one,two", max-age=90`}}, 90 * time.Second, true},
		{"overflow lifetime", http.Header{"Cache-Control": {"max-age=999999999999999999999999"}}, time.Hour, true},
		{"age consumes lifetime", http.Header{"Cache-Control": {"max-age=90"}, "Age": {"30"}}, time.Minute, true},
		{"legacy age does not shorten TTL", http.Header{"Age": {"99999"}}, time.Hour, true},
		{"expiry", http.Header{"Expires": {now.Add(time.Minute).Format(http.TimeFormat)}}, time.Minute, true},
		{"expiry with earlier date", http.Header{"Date": {now.Add(-time.Minute).Format(http.TimeFormat)}, "Expires": {now.Add(time.Minute).Format(http.TimeFormat)}}, time.Minute, true},
		{"expired date", http.Header{"Expires": {now.Add(-time.Minute).Format(http.TimeFormat)}}, 0, true},
		{"ignored expiry", http.Header{"Cache-Control": {"max-age=60"}, "Expires": {"invalid"}}, time.Minute, true},
		{"ignored ordinary numeric error", http.Header{"Cache-Control": {"max-age=invalid, s-maxage=60"}}, time.Minute, true},
		{"ignored ordinary duplicates", http.Header{"Cache-Control": {"max-age=1, max-age=2, s-maxage=60"}}, time.Minute, true},
		{"selected duplicates", http.Header{"Cache-Control": {"s-maxage=60, s-maxage=60"}}, 0, false},
		{"aliases duplicate", http.Header{"Cache-Control": {"max-age=60"}, "cache-control": {"MAX-AGE=60"}}, 0, false},
		{"fields duplicate", http.Header{"Cache-Control": {"max-age=60", "max-age=60"}}, 0, false},
		{"selected invalid", http.Header{"Cache-Control": {"s-maxage=no, max-age=60"}}, 0, false},
		{"missing argument", http.Header{"Cache-Control": {"max-age"}}, 0, false},
		{"invalid syntax", http.Header{"Cache-Control": {"max-age=60; private"}}, 0, false},
		{"empty argument", http.Header{"Cache-Control": {`max-age=""`}}, 0, false},
		{"signed lifetime", http.Header{"Cache-Control": {"max-age=+60"}}, 0, false},
		{"negative lifetime", http.Header{"Cache-Control": {"max-age=-60"}}, 0, false},
		{"fractional lifetime", http.Header{"Cache-Control": {"max-age=0.5"}}, 0, false},
		{"invalid date", http.Header{"Date": {"invalid"}}, 0, false},
		{"date aliases", http.Header{"Date": {now.Format(http.TimeFormat)}, "date": {now.Format(http.TimeFormat)}}, 0, false},
		{"empty date alias", http.Header{"Date": {now.Format(http.TimeFormat)}, "date": nil}, 0, false},
		{"empty date", http.Header{"Date": nil}, 0, false},
		{"duplicate expires", http.Header{"Expires": {now.Format(http.TimeFormat), now.Format(http.TimeFormat)}}, 0, false},
		{"invalid expires", http.Header{"Expires": {"0"}}, 0, false},
		{"quoted age", http.Header{"Age": {`"60"`}}, 0, false},
		{"age aliases", http.Header{"Age": {"1"}, "age": {"1"}}, 0, false},
		{"empty age alias", http.Header{"Age": {"1"}, "age": nil}, 0, false},
		{"empty age", http.Header{"Age": nil}, 0, false},
		{"comma age", http.Header{"Age": {"1, 1"}}, 0, false},
		{"policy trailer", http.Header{"Trailer": {"X-Other, cache-control"}}, 0, false},
		{"age trailer", http.Header{"Trailer": {"Age"}}, 0, false},
		{"date trailer", http.Header{"Trailer:Date": {now.Format(http.TimeFormat)}}, 0, false},
		{"expires trailer", http.Header{"Trailer:Expires": {now.Format(http.TimeFormat)}}, 0, false},
		{"ordinary trailer", http.Header{"Trailer": {"X-Checksum"}}, time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := cacheResponseFreshness(tc.header, now, now, time.Hour)
			if got.valid != tc.valid {
				t.Fatalf("valid = %v; want %v", got.valid, tc.valid)
			}
			if tc.valid && !got.expires.Equal(now.Add(tc.left)) {
				t.Errorf("remaining lifetime = %v; want %v", got.expires.Sub(now), tc.left)
			}
		})
	}
}

func TestCacheFreshnessCorrectedAge(t *testing.T) {
	t.Parallel()
	received := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		dateOffset time.Duration
		age        string
		wantAge    time.Duration
	}{
		{"response delay", 0, "10", 13 * time.Second},
		{"apparent age dominates", -30 * time.Second, "10", 30 * time.Second},
		{"future date", time.Hour, "10", 13 * time.Second},
		{"overflow age", 0, "999999999999999999999999999", time.Duration(math.MaxInt64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{"Date": {received.Add(tc.dateOffset).Format(http.TimeFormat)}, "Age": {tc.age}, "Cache-Control": {"max-age=60"}}
			got := cacheResponseFreshness(h, received.Add(-3*time.Second), received, time.Hour)
			if !got.valid || got.initialAge != tc.wantAge {
				t.Fatalf("freshness = %+v; want age %v", got, tc.wantAge)
			}
			if !got.expires.Equal(received.Add(time.Minute - tc.wantAge)) {
				t.Errorf("expiry = %v", got.expires)
			}
		})
	}
}

func TestCacheFreshnessReplayAndConstraint(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	f := cacheResponseFreshness(http.Header{"Age": {"10"}, "Cache-Control": {"max-age=60"}}, now, now, time.Hour)
	other := cacheResponseFreshness(http.Header{"Age": {"15"}, "Cache-Control": {"max-age=30"}}, now, now, time.Hour)
	constrained := f.constrain(other)
	if !constrained.valid || constrained.initialAge != 15*time.Second || !constrained.expires.Equal(now.Add(15*time.Second)) {
		t.Fatalf("constrained freshness = %+v", constrained)
	}
	h := http.Header{"Age": {"0"}, "aGe": {"999"}, "dAtE": {"old"}}
	constrained.apply(h, now.Add(2900*time.Millisecond))
	if h.Get("Age") != "17" || h.Get("Date") != now.Format(http.TimeFormat) || len(h) != 2 {
		t.Fatalf("replayed headers = %v", h)
	}
	if f.initialAge != 10*time.Second || !f.expires.Equal(now.Add(50*time.Second)) {
		t.Fatal("constraint mutated the original freshness")
	}
}

func TestCacheFreshnessInvalidConstraint(t *testing.T) {
	t.Parallel()
	now := time.Now()
	f := cacheResponseFreshness(nil, now, now, time.Hour)
	if f.constrain(cacheFreshness{}).valid {
		t.Fatal("invalid projection remained cacheable")
	}
}

func TestCacheFreshnessReplaySaturation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	f := cacheResponseFreshness(http.Header{"Age": {"9999999999999999999999"}}, now.Add(-time.Second), now, time.Hour)
	h := make(http.Header)
	f.apply(h, now.Add(time.Hour))
	if !f.valid || h.Get("Age") != "9223372036" {
		t.Fatalf("saturated Age = %q; valid = %v", h.Get("Age"), f.valid)
	}
}

func TestCacheFreshnessSeconds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		value string
		want  time.Duration
		valid bool
	}{
		{"0", 0, true},
		{"0003", 3 * time.Second, true},
		{"9223372036", 9223372036 * time.Second, true},
		{"9223372037", time.Duration(math.MaxInt64), true},
		{"9999999999999999999999999999999999", time.Duration(math.MaxInt64), true},
		{"999999999999999999999999999999999x", 0, false},
		{"", 0, false},
		{"-1", 0, false},
		{"1.5", 0, false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			got, valid := cacheFreshnessSeconds(tc.value)
			if got != tc.want || valid != tc.valid {
				t.Fatalf("seconds = (%v, %v); want (%v, %v)", got, valid, tc.want, tc.valid)
			}
		})
	}
}

func TestCacheFreshnessOriginZeroLifetime(t *testing.T) {
	t.Parallel()
	for _, policy := range []string{"max-age=0, must-revalidate", ""} {
		t.Run(policy, func(t *testing.T) {
			calls := 0
			h := cacheHandler(resolved.Middleware{Type: resolved.MWCache, CacheTTL: time.Hour}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.Header().Set("Cache-Control", policy)
				_, _ = w.Write([]byte("origin"))
			}))
			for range 2 {
				runRequest(t, h, httptest.NewRequest(http.MethodGet, "/", nil))
			}
			want := 2
			if policy == "" {
				want = 1
			}
			if calls != want {
				t.Fatalf("origin calls = %d; want %d", calls, want)
			}
		})
	}
}
