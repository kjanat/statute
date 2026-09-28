package cloudflare

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	minimumRefresh = 5 * time.Minute
	maximumRefresh = 24 * time.Hour
)

// refreshAfter treats s-maxage as a polling hint when max-age is absent.
// The transport remains an uncached HTTP client; this schedules revalidation.
func refreshAfter(header http.Header, received time.Time) time.Duration {
	directives := cacheDirectives(header.Values("Cache-Control"))
	if _, ok := directives["no-cache"]; ok {
		return minimumRefresh
	}
	if _, ok := directives["no-store"]; ok {
		return minimumRefresh
	}
	lifetime, ok := cacheLifetime(directives, header)
	if !ok {
		lifetime = maximumRefresh
	}
	age := cacheAge(header, received)
	if lifetime <= age {
		return minimumRefresh
	}
	return clampRefresh(lifetime - age)
}

// cacheDirectives retains duplicate directives as invalid values.
func cacheDirectives(values []string) map[string]string {
	directives := make(map[string]string)
	for _, value := range values {
		for directive := range strings.SplitSeq(value, ",") {
			key, val, _ := strings.Cut(strings.TrimSpace(directive), "=")
			key = strings.ToLower(strings.TrimSpace(key))
			if _, exists := directives[key]; exists {
				val = "invalid duplicate"
			}
			directives[key] = strings.Trim(strings.TrimSpace(val), "\"")
		}
	}
	return directives
}

// cacheLifetime gives the explicit client lifetime precedence over shared hints.
func cacheLifetime(directives map[string]string, header http.Header) (time.Duration, bool) {
	for _, key := range []string{"max-age", "s-maxage"} {
		if value, exists := directives[key]; exists {
			if duration, valid := secondsDuration(value); valid {
				return duration, true
			}
		}
	}
	date, dateErr := http.ParseTime(header.Get("Date"))
	expires, expiresErr := http.ParseTime(header.Get("Expires"))
	if dateErr != nil || expiresErr != nil {
		return 0, false
	}
	return expires.Sub(date), true
}

// secondsDuration rejects overflow and negative cache ages/lifetimes.
func secondsDuration(value string) (time.Duration, bool) {
	seconds, err := strconv.ParseUint(value, 10, 63)
	if err != nil || seconds > uint64((1<<63-1)/int64(time.Second)) {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}

// cacheAge uses the greater age estimate; Date and Age often describe the same
// time already spent in a shared cache and must not be added together.
func cacheAge(header http.Header, received time.Time) time.Duration {
	age, _ := secondsDuration(header.Get("Age"))
	if date, err := http.ParseTime(header.Get("Date")); err == nil {
		age = max(age, received.Sub(date))
	}
	return max(0, age)
}

// clampRefresh prevents provider headers from causing a busy loop or stale days.
func clampRefresh(delay time.Duration) time.Duration {
	return max(minimumRefresh, min(maximumRefresh, delay))
}
