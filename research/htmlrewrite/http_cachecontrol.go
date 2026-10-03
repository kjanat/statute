package htmlrewrite

import (
	"net/http"
	"strings"

	"golang.org/x/net/http/httpguts"
)

// cacheControlPermitsTransform checks request and response directives using the
// same grammar. Unknown extensions are allowed; unreadable policy is not. The
// caller applies its explicit failure policy before consuming any HTML bytes.
func cacheControlPermitsTransform(h http.Header) bool {
	for name, values := range h {
		if !strings.EqualFold(name, "Cache-Control") {
			continue
		}
		for _, value := range values {
			if !cacheFieldPermitsTransform(value) {
				return false
			}
		}
	}
	return true
}

func cacheFieldPermitsTransform(value string) bool {
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
		if !httpguts.ValidHeaderFieldName(name) || strings.EqualFold(name, "no-transform") {
			return false
		}
		value = strings.TrimLeft(value[end:], " \t")
		if strings.HasPrefix(value, "=") {
			var ok bool
			value, ok = consumeCacheValue(strings.TrimLeft(value[1:], " \t"))
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

// consumeCacheValue preserves quoted commas and escaped quotes (RFC 9111
// section 5.2). A directive-looking extension value is not a directive.
func consumeCacheValue(value string) (rest string, ok bool) {
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
