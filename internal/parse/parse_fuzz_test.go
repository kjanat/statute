package parse

import (
	"math"
	"strings"
	"testing"
)

// FuzzParseDuration invariant: never panic; on err==nil the result is
// non-negative. Duration accepts arbitrarily long durations (Go's
// time.Duration is int64 nanoseconds — about 292 years before overflow) and
// the user is responsible for choosing sensible values. We don't make a
// "too long" judgment here.
func FuzzParseDuration(f *testing.F) {
	for _, seed := range []string{"5s", "90s", "1h", "500ms", "0", "0s", "1.5h", "-1ns", "  ", "", "abc", "1y", "1d", "70000d"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		d, err := Duration(s)
		if err != nil {
			return
		}
		if d < 0 {
			t.Errorf("Duration(%q) = %v; negative duration", s, d)
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
		r, err := Rate(s)
		if err != nil {
			return
		}
		if r <= 0 {
			t.Errorf("Rate(%q) = %v; non-positive rate", s, r)
		}
		if math.IsInf(r, 0) || math.IsNaN(r) {
			t.Errorf("Rate(%q) = %v; not finite", s, r)
		}
	})
}

// FuzzParseSize invariant: never panic; on err==nil the result is non-negative.
func FuzzParseSize(f *testing.F) {
	for _, seed := range []string{"100", "1KB", "1MB", "1GB", "1KiB", "1MiB", "1GiB", "1.5GB", "0", "", "abc", "1XB", "-5MB", "10000000000000000000", "1e308GB", "9223372036854775807"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		n, err := Size(s)
		if err != nil {
			return
		}
		if n < 0 {
			t.Errorf("Size(%q) = %d; negative", s, n)
		}
	})
}

// FuzzParseHeaderName invariant: never panic; on err==nil the result is a
// non-empty, canonical field name that survives a second pass unchanged, and
// carries nothing that could split a header line.
func FuzzParseHeaderName(f *testing.F) {
	for _, seed := range []string{"x-robots-tag", "Content-Type", "", "X Robots", "X-Robots:", "a\r\nb", "!#$%&'*+-.^_`|~", "héader"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		name, err := HeaderName(s)
		if err != nil {
			return
		}
		if name == "" {
			t.Errorf("HeaderName(%q) = %q; empty name accepted", s, name)
		}
		if strings.ContainsAny(name, "\r\n\x00 :") {
			t.Errorf("HeaderName(%q) = %q; name can split a header line", s, name)
		}
		again, err := HeaderName(name)
		if err != nil || again != name {
			t.Errorf("HeaderName(%q) = %q, not canonical: second pass gave %q, %v", s, name, again, err)
		}
	})
}

// FuzzParseHeaderValue invariant: never panic; on err==nil the value is
// returned unchanged and carries no control byte that could forge a header.
func FuzzParseHeaderValue(f *testing.F) {
	for _, seed := range []string{"noindex, nofollow", "", "tab\there", "a\r\nX-Injected: yes", "nul\x00", "del\x7f"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got, err := HeaderValue(s)
		if err != nil {
			return
		}
		if got != s {
			t.Errorf("HeaderValue(%q) = %q; value was modified", s, got)
		}
		if strings.ContainsAny(got, "\r\n\x00") {
			t.Errorf("HeaderValue(%q) accepted a control byte", s)
		}
	})
}
