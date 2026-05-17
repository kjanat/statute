package statute

import (
	"math"
	"testing"
)

// FuzzParseDuration invariant: never panic; on err==nil the result is
// non-negative. parseDuration accepts arbitrarily long durations (Go's
// time.Duration is int64 nanoseconds — about 292 years before overflow) and
// the user is responsible for choosing sensible values. We don't make a
// "too long" judgment here.
func FuzzParseDuration(f *testing.F) {
	for _, seed := range []string{"5s", "90s", "1h", "500ms", "0", "0s", "1.5h", "-1ns", "  ", "", "abc", "1y", "1d", "70000d"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		d, err := parseDuration(s)
		if err != nil {
			return
		}
		if d < 0 {
			t.Errorf("parseDuration(%q) = %v; negative duration", s, d)
		}
	})
}

// FuzzParseRate invariant: never panic; on err==nil the result is finite
// and strictly positive. Overflow to +Inf would be a real bug.
func FuzzParseRate(f *testing.F) {
	for _, seed := range []string{"1/s", "60/min", "100/h", "0/min", "-1/s", "abc/min", "5", "5/", "/min"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		r, err := parseRate(s)
		if err != nil {
			return
		}
		if r <= 0 {
			t.Errorf("parseRate(%q) = %v; non-positive rate", s, r)
		}
		if math.IsInf(r, 0) || math.IsNaN(r) {
			t.Errorf("parseRate(%q) = %v; not finite", s, r)
		}
	})
}

// FuzzParseSize invariant: never panic; on err==nil the result is non-negative.
func FuzzParseSize(f *testing.F) {
	for _, seed := range []string{"100", "1KB", "1MB", "1GB", "1KiB", "1MiB", "1GiB", "1.5GB", "0", "", "abc", "1XB", "-5MB", "10000000000000000000", "1e308GB", "9223372036854775807"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		n, err := parseSize(s)
		if err != nil {
			return
		}
		if n < 0 {
			t.Errorf("parseSize(%q) = %d; negative", s, n)
		}
	})
}
