package statute

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type cacheFreshness struct {
	received   time.Time
	date       time.Time
	initialAge time.Duration
	expires    time.Time
	valid      bool
}

func cacheResponseFreshness(h http.Header, requested, received time.Time, ttl time.Duration) cacheFreshness {
	f := cacheFreshness{received: received, expires: received.Add(ttl)}
	if !cacheControlAllows(h) || cacheFreshnessTrailers(h) {
		return f
	}
	date, dateOK := cacheFreshnessDate(h, "Date", received)
	age, ageOK := cacheFreshnessAge(h)
	lifetime, explicit, lifetimeOK := cacheFreshnessLifetime(h, date)
	if !dateOK || !ageOK || !lifetimeOK {
		return f
	}
	f.date = date
	f.initialAge = max(max(time.Duration(0), received.Sub(date)), cacheFreshnessAdd(age, max(time.Duration(0), received.Sub(requested))))
	if explicit {
		f.expires = minTime(f.expires, received.Add(lifetime-f.initialAge))
	}
	f.valid = true
	return f
}

func (f cacheFreshness) constrain(other cacheFreshness) cacheFreshness {
	f.valid = f.valid && other.valid
	f.initialAge = max(f.initialAge, other.initialAge)
	f.expires = minTime(f.expires, other.expires)
	return f
}

func (f cacheFreshness) apply(h http.Header, now time.Time) {
	for name := range h {
		if strings.EqualFold(name, "Age") || strings.EqualFold(name, "Date") {
			delete(h, name)
		}
	}
	age := cacheFreshnessAdd(f.initialAge, max(time.Duration(0), now.Sub(f.received)))
	h.Set("Age", strconv.FormatInt(int64(age/time.Second), 10))
	h.Set("Date", f.date.UTC().Format(http.TimeFormat))
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func cacheFreshnessAdd(a, b time.Duration) time.Duration {
	if a > time.Duration(math.MaxInt64)-b {
		return time.Duration(math.MaxInt64)
	}
	return a + b
}

func cacheFreshnessDate(h http.Header, name string, fallback time.Time) (time.Time, bool) {
	value, present, valid := cacheFreshnessSingleHeader(h, name)
	if !present {
		return fallback, true
	}
	if !valid {
		return time.Time{}, false
	}
	date, err := http.ParseTime(strings.TrimSpace(value))
	return date, err == nil
}

func cacheFreshnessAge(h http.Header) (time.Duration, bool) {
	value, present, valid := cacheFreshnessSingleHeader(h, "Age")
	if !present {
		return 0, true
	}
	if !valid {
		return 0, false
	}
	return cacheFreshnessSeconds(strings.TrimSpace(value))
}

func cacheFreshnessSingleHeader(h http.Header, name string) (value string, present, valid bool) {
	valid = true
	for key, values := range h {
		if !strings.EqualFold(key, name) {
			continue
		}
		if present || len(values) != 1 {
			valid = false
		} else {
			value = values[0]
		}
		present = true
	}
	return value, present, valid
}

func cacheFreshnessSeconds(value string) (time.Duration, bool) {
	if value == "" {
		return 0, false
	}
	var seconds int64
	const ceiling = math.MaxInt64 / int64(time.Second)
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return 0, false
		}
		n := int64(digit - '0')
		if seconds > (ceiling-n)/10 {
			seconds = ceiling + 1
		} else {
			seconds = seconds*10 + n
		}
	}
	if seconds > ceiling {
		return time.Duration(math.MaxInt64), true
	}
	return time.Duration(seconds) * time.Second, true
}

type cacheFreshnessDirective struct {
	value string
	count int
}

func cacheFreshnessLifetime(h http.Header, date time.Time) (time.Duration, bool, bool) {
	shared, ordinary := cacheFreshnessDirectives(h)
	selected := ordinary
	if shared.count != 0 {
		selected = shared
	}
	if selected.count != 0 {
		value := selected.value
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			value = value[1 : len(value)-1]
		}
		lifetime, ok := cacheFreshnessSeconds(value)
		return lifetime, true, ok && selected.count == 1
	}
	expires, ok := cacheFreshnessDate(h, "Expires", date)
	_, explicit, _ := cacheFreshnessSingleHeader(h, "Expires")
	return max(time.Duration(0), expires.Sub(date)), explicit, ok
}

func cacheFreshnessDirectives(h http.Header) (shared, ordinary cacheFreshnessDirective) {
	values, _ := cacheHeaderValues(h, "Cache-Control")
	for _, value := range values {
		for value != "" {
			var name, argument string
			name, argument, value = cacheFreshnessDirectiveNext(value)
			switch {
			case strings.EqualFold(name, "s-maxage"):
				shared.value, shared.count = argument, shared.count+1
			case strings.EqualFold(name, "max-age"):
				ordinary.value, ordinary.count = argument, ordinary.count+1
			}
		}
	}
	return shared, ordinary
}

// cacheFreshnessDirectiveNext consumes syntax already checked by cacheControlAllows.
func cacheFreshnessDirectiveNext(value string) (name, argument, rest string) {
	value = strings.TrimLeft(value, " \t,")
	end := strings.IndexAny(value, "=, \t")
	if end < 0 {
		return value, "", ""
	}
	name = value[:end]
	rest = strings.TrimLeft(value[end:], " \t")
	if strings.HasPrefix(rest, "=") {
		value = strings.TrimLeft(rest[1:], " \t")
		rest, _ = consumeCacheArgument(value)
		argument = value[:len(value)-len(rest)]
	}
	return name, argument, strings.TrimLeft(rest, " \t,")
}

func cacheFreshnessTrailers(h http.Header) bool {
	for name, values := range h {
		if strings.HasPrefix(name, http.TrailerPrefix) && cacheFreshnessField(strings.TrimPrefix(name, http.TrailerPrefix)) {
			return true
		}
		if strings.EqualFold(name, "Trailer") && cacheFreshnessTrailerValues(values) {
			return true
		}
	}
	return false
}

func cacheFreshnessTrailerValues(values []string) bool {
	for _, value := range values {
		for name := range strings.SplitSeq(value, ",") {
			if cacheFreshnessField(strings.TrimSpace(name)) {
				return true
			}
		}
	}
	return false
}

func cacheFreshnessField(name string) bool {
	return strings.EqualFold(name, "Cache-Control") || strings.EqualFold(name, "Age") || strings.EqualFold(name, "Date") || strings.EqualFold(name, "Expires")
}
