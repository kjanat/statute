package parse

import (
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		err  bool
	}{
		{"5s", 5 * time.Second, false},
		{"90s", 90 * time.Second, false},
		{"1h", time.Hour, false},
		{"500ms", 500 * time.Millisecond, false},
		{"1d", 24 * time.Hour, false},
		{"2d", 48 * time.Hour, false},
		{"1w", 7 * 24 * time.Hour, false},
		{"1w2d", 9 * 24 * time.Hour, false},
		{"1w2d3h", 9*24*time.Hour + 3*time.Hour, false},
		{"1.5d", 36 * time.Hour, false},
		{".5d", 12 * time.Hour, false},   // leading-dot day, like 0.5d
		{".5w", 84 * time.Hour, false},   // 0.5 * 168h
		{"+.5d", 12 * time.Hour, false},  // signed leading-dot
		{".5h", 30 * time.Minute, false}, // leading dot, non-d/w unit
		{"0d", 0, false},
		{"-1d", 0, true},
		{".", 0, true},      // lone dot is not a number
		{".d", 0, true},     // dot not followed by digit
		{"1.2.3d", 0, true}, // bad float before the d/w unit
		{"+s", 0, true},     // lone sign, not a number token
		{"-1s", 0, true},
		{"forever", 0, true},
		{"", 0, true},
	}
	for _, c := range cases {
		got, err := Duration(c.in)
		if c.err {
			if err == nil {
				t.Errorf("Duration(%q) = %v, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("Duration(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("Duration(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseDurationOr(t *testing.T) {
	got, err := DurationOr("", 7*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got != 7*time.Second {
		t.Errorf("empty fallback: got %v, want 7s", got)
	}
	got, err = DurationOr("250ms", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got != 250*time.Millisecond {
		t.Errorf("explicit value: got %v", got)
	}
}

func TestParseRate(t *testing.T) {
	cases := []struct {
		in   string
		want float64
		err  bool
	}{
		{"1/s", 1, false},
		{"60/min", 1, false},
		{"3600/h", 1, false},
		{"3600/hr", 1, false},
		{"3600/hour", 1, false},
		{"3600/hours", 1, false},
		{"100/sec", 100, false},
		{"100/seconds", 100, false},
		{"100/minutes", 100.0 / 60, false},
		{"100/x", 0, true},
		{"abc/min", 0, true},
		{"-5/min", 0, true},
		{"5", 0, true},
		{"NaN/s", 0, true},
		{"Inf/s", 0, true},
		{"+Inf/min", 0, true},
		{"-Inf/s", 0, true},
	}
	for _, c := range cases {
		got, err := Rate(c.in)
		if c.err {
			if err == nil {
				t.Errorf("Rate(%q) = %v, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("Rate(%q) error: %v", c.in, err)
			continue
		}
		if got < c.want-1e-9 || got > c.want+1e-9 {
			t.Errorf("Rate(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseSize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want int64
		err  bool
	}{
		{"1", 1, false},
		{"100", 100, false},
		{"1KB", 1000, false},
		{"1KiB", 1024, false},
		{"2MB", 2_000_000, false},
		{"2MiB", 2 * 1024 * 1024, false},
		{"1.5GB", 1_500_000_000, false},
		{"512B", 512, false},
		{"  10kb  ", 10_000, false},
		{"1TB", 1_000_000_000_000, false},
		{"1TiB", 1024 * 1024 * 1024 * 1024, false},
		{"1PB", 1_000_000_000_000_000, false},
		{"9EB", 9_000_000_000_000_000_000, false}, // < MaxInt64
		{"10EB", 0, true},                         // 1e19 >= MaxInt64
		{"1QB", 0, true},                          // 1000^10, far too large
		{"abc", 0, true},
		{"-1MB", 0, true},
		{"1.2.3KB", 0, true}, // numeric run present but not a valid float
		{"1XB", 0, true},
		{"ib", 0, true},
		{"", 0, true},
	}
	for _, c := range cases {
		got, err := Size(c.in)
		if c.err {
			if err == nil {
				t.Errorf("Size(%q) = %d, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("Size(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("Size(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestHeaderName(t *testing.T) {
	cases := []struct {
		in   string
		want string
		err  bool
	}{
		{"x-robots-tag", "X-Robots-Tag", false},
		{"X-Robots-Tag", "X-Robots-Tag", false},
		{"CONTENT-TYPE", "Content-Type", false},
		{"x_custom", "X_custom", false}, // underscore is a token char, so no dash-casing
		{"a", "A", false},
		{"!#$%&'*+-.^_`|~", "!#$%&'*+-.^_`|~", false}, // every tchar symbol
		{"", "", true},
		{"X Robots", "", true},
		{"X-Robots:", "", true},
		{"X-Robots\n", "", true},
		{"héader", "", true},
	}
	for _, c := range cases {
		got, err := HeaderName(c.in)
		if c.err {
			if err == nil {
				t.Errorf("HeaderName(%q) = %q, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("HeaderName(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("HeaderName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHeaderValue(t *testing.T) {
	cases := []struct {
		in  string
		err bool
	}{
		{"noindex, nofollow", false},
		{"", false},
		{"tab\there", false},
		{"ünicode obs-text", false},
		{"noindex\r\nX-Injected: yes", true},
		{"trailing\n", true},
		{"nul\x00byte", true},
		{"del\x7f", true},
	}
	for _, c := range cases {
		got, err := HeaderValue(c.in)
		if c.err {
			if err == nil {
				t.Errorf("HeaderValue(%q) = %q, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("HeaderValue(%q): %v", c.in, err)
			continue
		}
		if got != c.in {
			t.Errorf("HeaderValue(%q) = %q, want it unchanged", c.in, got)
		}
	}
}

func TestStatusRange(t *testing.T) {
	cases := []struct {
		in     string
		lo, hi int
		err    bool
	}{
		{"400-499", 400, 499, false},
		{"500-599", 500, 599, false},
		{"404", 404, 404, false},
		{"100-599", 100, 599, false},
		{" 200 - 299 ", 200, 299, false},
		{"", 0, 0, true},
		{"-499", 0, 0, true},
		{"400-", 0, 0, true},
		{"499-400", 0, 0, true},
		{"99", 0, 0, true},
		{"600", 0, 0, true},
		{"100-600", 0, 0, true},
		{"4xx", 0, 0, true},
		{"200-299-399", 0, 0, true},
	}
	for _, c := range cases {
		lo, hi, err := StatusRange(c.in)
		if c.err {
			if err == nil {
				t.Errorf("StatusRange(%q) = %d-%d, want error", c.in, lo, hi)
			}
			continue
		}
		if err != nil {
			t.Errorf("StatusRange(%q): %v", c.in, err)
			continue
		}
		if lo != c.lo || hi != c.hi {
			t.Errorf("StatusRange(%q) = %d-%d, want %d-%d", c.in, lo, hi, c.lo, c.hi)
		}
	}
}
