package statute

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
		got, err := parseDuration(c.in)
		if c.err {
			if err == nil {
				t.Errorf("parseDuration(%q) = %v, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseDuration(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseDuration(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseDurationOr(t *testing.T) {
	got, err := parseDurationOr("", 7*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got != 7*time.Second {
		t.Errorf("empty fallback: got %v, want 7s", got)
	}
	got, err = parseDurationOr("250ms", time.Hour)
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
		got, err := parseRate(c.in)
		if c.err {
			if err == nil {
				t.Errorf("parseRate(%q) = %v, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseRate(%q) error: %v", c.in, err)
			continue
		}
		if got < c.want-1e-9 || got > c.want+1e-9 {
			t.Errorf("parseRate(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
