// Package parse holds the string-to-value parsers for statute's duration,
// rate, and byte-size config fields. The functions are pure: they take a
// string and return a primitive or an error, with no dependency on the
// surface or resolved config types. Kept in internal/ so they are not part
// of the public API surface.
package parse

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// DurationOr parses s, or returns fallback when s is empty.
func DurationOr(s string, fallback time.Duration) (time.Duration, error) {
	if s == "" {
		return fallback, nil
	}
	return Duration(s)
}

// Duration accepts every unit Go's time.ParseDuration accepts (ns, us,
// ms, s, m, h) plus "d" for days (24h) and "w" for weeks (7d). Days and
// weeks are de-sugared by string-rewriting before falling through to the
// stdlib parser, so they compose with the other units ("1w2d" works).
func Duration(s string) (time.Duration, error) {
	normalized, err := expandDayWeekUnits(s)
	if err != nil {
		return 0, err
	}
	d, err := time.ParseDuration(normalized)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", s, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("duration %q is negative", s)
	}
	return d, nil
}

// expandDayWeekUnits rewrites Nd → N*24h and Nw → N*168h. The rewrite is
// purely textual — it requires the number to immediately precede the unit
// suffix and falls through to a plain ParseDuration error otherwise.
func expandDayWeekUnits(s string) (string, error) {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		c := s[i]
		// Capture a number prefix (with optional sign + fractional part).
		if isSign(c) || isDigit(c) {
			next, err := expandNumberAt(s, i, &b)
			if err != nil {
				return "", err
			}
			i = next
			continue
		}
		b.WriteByte(c)
		i++
	}
	return b.String(), nil
}

// isDigit reports whether c is an ASCII digit.
func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// isSign reports whether c is an ASCII plus or minus sign.
func isSign(c byte) bool { return c == '-' || c == '+' }

// expandNumberAt processes the token beginning at s[i] (a digit or sign),
// appending the rewritten form to b, and returns the index just past the
// consumed bytes. A trailing d/w unit is expanded to hours; any other number
// is copied verbatim. If s[i] does not actually begin a number, the single
// byte is copied and i+1 is returned.
func expandNumberAt(s string, i int, b *strings.Builder) (int, error) {
	j := i
	if isSign(s[i]) {
		j++
	}
	j = scanDigits(s, j)
	if !isNumberToken(s, i, j) {
		// Not actually a number; copy the single byte.
		b.WriteByte(s[i])
		return i + 1, nil
	}
	if j < len(s) && isDayWeekUnit(s[j]) {
		return expandDayWeek(s, i, j, b)
	}
	// No d/w suffix — copy the captured number verbatim.
	b.WriteString(s[i:j])
	return j, nil
}

// scanDigits returns the index past the run of digits and dots starting at j.
func scanDigits(s string, j int) int {
	for j < len(s) && (isDigit(s[j]) || s[j] == '.') {
		j++
	}
	return j
}

// isNumberToken reports whether s[i:j] is a real number rather than a lone
// sign that was never followed by digits.
func isNumberToken(s string, i, j int) bool {
	if j == i {
		return false
	}
	if j == i+1 && isSign(s[i]) {
		return false
	}
	return true
}

// isDayWeekUnit reports whether c is the 'd' (day) or 'w' (week) suffix.
func isDayWeekUnit(c byte) bool { return c == 'd' || c == 'w' }

// expandDayWeek rewrites the number s[i:j] followed by the unit at s[j]
// ('d' → 24h, 'w' → 168h) into an "<hours>h" string appended to b. It
// returns the index just past the consumed unit byte.
func expandDayWeek(s string, i, j int, b *strings.Builder) (int, error) {
	num := s[i:j]
	hours := 24
	if s[j] == 'w' {
		hours = 24 * 7
	}
	n, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("duration %q: invalid number %q: %w", s, num, err)
	}
	h := n * float64(hours)
	// Emit as "<h>h" so time.ParseDuration handles it.
	b.WriteString(strconv.FormatFloat(h, 'f', -1, 64))
	b.WriteByte('h')
	return j + 1, nil
}

// Rate parses a rate of the form "N/unit" into requests per second.
// Supported units: s, sec, second; m, min, minute; h, hr, hour.
func Rate(s string) (float64, error) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return 0, fmt.Errorf("rate %q must be N/unit", s)
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	if err != nil {
		return 0, fmt.Errorf("rate %q: invalid count: %w", s, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("rate %q: count must be positive", s)
	}
	switch strings.ToLower(strings.TrimSpace(parts[1])) {
	case "s", "sec", "second", "seconds":
		return n, nil
	case "m", "min", "minute", "minutes":
		return n / 60, nil
	case "h", "hr", "hour", "hours":
		return n / 3600, nil
	default:
		return 0, fmt.Errorf("rate %q: unknown unit %q", s, parts[1])
	}
}

// Size parses a byte size like "1MB", "512KiB", or "256" into a count
// of bytes. Suffixes are case-insensitive. Decimal (KB/MB/GB) and binary
// (KiB/MiB/GiB) units are both accepted. Decimal units use powers of 1000;
// binary units use powers of 1024.
func Size(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("size: empty")
	}
	numStr, unit := splitSizeUnit(s)
	if numStr == "" {
		return 0, fmt.Errorf("size %q: missing number", s)
	}
	n, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: invalid number: %w", s, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("size %q: negative", s)
	}
	mult, err := sizeMultiplier(unit)
	if err != nil {
		return 0, fmt.Errorf("size %q: unknown unit %q", s, unit)
	}
	bytes := n * mult
	if math.IsInf(bytes, 0) || math.IsNaN(bytes) || bytes >= math.MaxInt64 {
		return 0, fmt.Errorf("size %q: too large", s)
	}
	return int64(bytes), nil
}

// splitSizeUnit splits a trimmed size string at the boundary between the
// leading numeric run (digits, dot, sign) and the trailing unit. The unit is
// returned lower-cased so the multiplier lookup is case-insensitive.
func splitSizeUnit(s string) (numStr, unit string) {
	i := 0
	for i < len(s) {
		c := s[i]
		if isDigit(c) || c == '.' || isSign(c) {
			i++
			continue
		}
		break
	}
	return strings.TrimSpace(s[:i]), strings.ToLower(strings.TrimSpace(s[i:]))
}

// bytePrefixes is the ordered list of byte-size prefixes; a prefix's index
// is its exponent minus one. Decimal units raise 1000 to that exponent,
// binary units (…ib) raise 1024. Extending the range (terabytes,
// petabytes, …) is just a longer string — no new mapping.
const bytePrefixes = "kmgtpezyrq"

// sizeMultiplier maps a lower-cased byte-size unit to its multiplier.
// Decimal units (kb, mb, gb, tb, …) use powers of 1000; binary units
// (kib, mib, gib, …) use powers of 1024. The bare unit "b" (or "") is 1.
// Case is irrelevant here because callers lower-case the unit before this
// sees it; this is byte-size parsing, not general SI (where m=milli≠M=mega).
// The unit arrives already trimmed and lower-cased from splitSizeUnit, so
// this does no further normalization.
func sizeMultiplier(unit string) (float64, error) {
	if unit == "" || unit == "b" {
		return 1, nil
	}

	base := 1000.0
	prefix := unit
	switch {
	case strings.HasSuffix(unit, "ib"):
		base = 1024
		prefix = strings.TrimSuffix(unit, "ib")
	case strings.HasSuffix(unit, "b"):
		prefix = strings.TrimSuffix(unit, "b")
	}

	if len(prefix) != 1 {
		return 0, fmt.Errorf("unknown unit %q", unit)
	}
	idx := strings.IndexByte(bytePrefixes, prefix[0])
	if idx == -1 {
		return 0, fmt.Errorf("unknown unit %q", unit)
	}
	return math.Pow(base, float64(idx+1)), nil
}
