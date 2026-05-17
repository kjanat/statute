package statute

import (
	"testing"
	"time"
)

// FuzzParseDuration — invariant: never panic; on err==nil, result is
// non-negative finite.
func FuzzParseDuration(f *testing.F) {
	for _, seed := range []string{"5s", "90s", "1h", "500ms", "0", "0s", "1.5h", "-1ns", "  ", "", "abc", "1y"} {
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
		if d > 100*365*24*time.Hour {
			t.Errorf("parseDuration(%q) = %v; absurdly large duration", s, d)
		}
	})
}

// FuzzParseRate — invariant: never panic; on err==nil, result is a finite
// positive rate.
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
		// 1 billion requests per second is absurd in any reasonable config.
		// If we accept that, something is wrong with bounds.
		if r > 1e9 {
			t.Errorf("parseRate(%q) = %v; absurdly large rate", s, r)
		}
	})
}
