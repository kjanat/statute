// Package httpprecondition evaluates read preconditions against a selected
// representation. Its caller must first establish an otherwise successful read.
package httpprecondition

import (
	"net/http"
	"strings"
)

// Clear removes preconditions and ranges from a caller-owned render request.
// The original request is retained separately for final-representation checks.
func Clear(h http.Header) {
	for name := range h {
		switch strings.ToLower(name) {
		case "if-match", "if-none-match", "if-modified-since", "if-unmodified-since", "range", "if-range":
			delete(h, name)
		}
	}
}

// Status returns zero to deliver the selected GET/HEAD representation, or the
// bodyless status to send instead. Malformed entity-tag lists yield 400; invalid
// dates are ignored. Field precedence follows RFC 9110 section 13.2.2.
func Status(request, selected http.Header) int {
	current, _ := field(selected, "ETag")
	match, hasMatch := field(request, "If-Match")
	if hasMatch {
		matches, valid := matchTags(match, current, false)
		if !valid {
			return http.StatusBadRequest
		}
		if !matches {
			return http.StatusPreconditionFailed
		}
	} else if modifiedAfter(selected, request, "If-Unmodified-Since") {
		return http.StatusPreconditionFailed
	}
	none, hasNone := field(request, "If-None-Match")
	if hasNone {
		matches, valid := matchTags(none, current, true)
		if !valid {
			return http.StatusBadRequest
		}
		if matches {
			return http.StatusNotModified
		}
	} else if unmodifiedSince(selected, request) {
		return http.StatusNotModified
	}
	return 0
}

func field(h http.Header, wanted string) (string, bool) {
	var values []string
	present := false
	for name, fields := range h {
		if strings.EqualFold(name, wanted) {
			values = append(values, fields...)
			present = true
		}
	}
	return strings.Join(values, ","), present
}

func modifiedAfter(selected, request http.Header, condition string) bool {
	modified, _ := field(selected, "Last-Modified")
	since, _ := field(request, condition)
	mt, merr := http.ParseTime(modified)
	st, serr := http.ParseTime(since)
	return merr == nil && serr == nil && mt.After(st)
}

func unmodifiedSince(selected, request http.Header) bool {
	modified, _ := field(selected, "Last-Modified")
	since, _ := field(request, "If-Modified-Since")
	mt, merr := http.ParseTime(modified)
	st, serr := http.ParseTime(since)
	return merr == nil && serr == nil && !mt.After(st)
}

func matchTags(value, current string, weak bool) (bool, bool) {
	value = strings.Trim(value, " \t")
	if value == "*" {
		return true, true // The caller established a current representation.
	}
	currentTag, rest := consumeTag(strings.Trim(current, " \t"))
	if strings.Trim(rest, " \t") != "" {
		currentTag = ""
	}
	matched, anyTag := false, false
	for {
		value = strings.TrimLeft(value, " \t,")
		if value == "" {
			return matched, anyTag
		}
		tag, rest := consumeTag(value)
		if tag == "" {
			return false, false
		}
		anyTag = true
		matched = matched || tagsEqual(tag, currentTag, weak)
		value = strings.TrimLeft(rest, " \t")
		if value != "" && value[0] != ',' {
			return false, false
		}
	}
}

func tagsEqual(a, b string, weak bool) bool {
	if weak {
		return strings.TrimPrefix(a, "W/") == strings.TrimPrefix(b, "W/")
	}
	return !strings.HasPrefix(a, "W/") && !strings.HasPrefix(b, "W/") && a == b
}

// Entity-tag backslashes are literal bytes (RFC 9110 section 8.8.3).
func consumeTag(value string) (string, string) {
	start := 0
	if strings.HasPrefix(value, "W/") {
		start = 2
	}
	if len(value) <= start || value[start] != '"' {
		return "", value
	}
	for i := start + 1; i < len(value); i++ {
		if value[i] == '"' {
			return value[:i+1], value[i+1:]
		}
		if value[i] < 0x21 || value[i] == 0x7f {
			return "", value
		}
	}
	return "", value
}
