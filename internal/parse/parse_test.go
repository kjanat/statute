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
		{"100/sec", 100, false},
		{"100/seconds", 100, false},
		{"100/minutes", 100.0 / 60, false},
		{"100/x", 0, true},
		{"abc/min", 0, true},
		{"-5/min", 0, true},
		{"5", 0, true},
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
