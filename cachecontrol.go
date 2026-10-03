package statute

import (
	"net/http"
	"slices"
	"strings"

	"golang.org/x/net/http/httpguts"
)

// cacheControlAllowsStorage checks no-store across all field values. Invalid
// syntax declines storage; unknown well-formed extensions leave it unchanged.
func cacheControlAllowsStorage(h http.Header) bool {
	return cacheControlAllows(h, "no-store")
}

func cacheControlAllows(h http.Header, forbidden ...string) bool {
	for name, values := range h {
		if strings.EqualFold(name, "Cache-Control") {
			for _, value := range values {
				if !cacheControlFieldAllows(value, forbidden...) {
					return false
				}
			}
		}
	}
	return true
}

func cacheControlFieldAllowsStorage(value string) bool {
	return cacheControlFieldAllows(value, "no-store")
}

func cacheControlFieldAllows(value string, forbidden ...string) bool {
	for {
		value = strings.TrimLeft(value, " \t,")
		if value == "" {
			return true
		}
		end := strings.IndexAny(value, "=, \t")
		if end < 0 {
			end = len(value)
		}
		name := value[:end]
		if !httpguts.ValidHeaderFieldName(name) || slices.ContainsFunc(forbidden, func(field string) bool { return strings.EqualFold(name, field) }) {
			return false
		}
		value = strings.TrimLeft(value[end:], " \t")
		if strings.HasPrefix(value, "=") {
			var ok bool
			value, ok = consumeCacheArgument(strings.TrimLeft(value[1:], " \t"))
			if !ok {
				return false
			}
			value = strings.TrimLeft(value, " \t")
		}
		if value != "" && value[0] != ',' {
			return false
		}
	}
}

// consumeCacheArgument consumes one token or quoted-string, including escaped
// quotes and commas inside extension arguments (RFC 9111 section 5.2).
func consumeCacheArgument(value string) (rest string, ok bool) {
	if !strings.HasPrefix(value, `"`) {
		end := strings.IndexAny(value, ", \t")
		if end < 0 {
			end = len(value)
		}
		return value[end:], httpguts.ValidHeaderFieldName(value[:end])
	}
	for i := 1; i < len(value); i++ {
		switch value[i] {
		case '"':
			return value[i+1:], true
		case '\\':
			i++
			if i == len(value) {
				return "", false
			}
		}
		if (value[i] < ' ' && value[i] != '\t') || value[i] == 0x7f {
			return "", false
		}
	}
	return "", false
}
