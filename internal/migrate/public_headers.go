package migrate

import (
	"slices"
	"strconv"
	"strings"
)

// These standard policy values contain no credential material. Arbitrary header
// values require environment bindings.
func publicHeaderValue(name, value string) bool {
	switch strings.ToLower(name) {
	case frameOptionsHeader:
		return slices.Contains([]string{frameDenyValue, "SAMEORIGIN"}, value)
	case "x-xss-protection":
		return slices.Contains([]string{"0", "1", xssBlockValue}, value)
	case "x-forwarded-proto":
		return slices.Contains([]string{"http", "https", "ws", "wss"}, value)
	case "x-robots-tag":
		return publicRobotsHeader(value)
	case "cache-control":
		return publicCacheControl(value)
	}
	return false
}

func publicRobotsHeader(value string) bool {
	for item := range strings.SplitSeq(value, ",") {
		switch strings.TrimSpace(strings.ToLower(item)) {
		case "all", "none", "noindex", "index", "nofollow", "follow", "noarchive", "nosnippet", "noimageindex", "notranslate", "nocache":
		default:
			return false
		}
	}
	return value != ""
}

func publicCacheControl(value string) bool {
	for item := range strings.SplitSeq(value, ",") {
		item = strings.TrimSpace(strings.ToLower(item))
		switch item {
		case "public", "private", "no-cache", "no-store", "must-revalidate", "proxy-revalidate", "immutable", "no-transform":
			continue
		}
		key, number, ok := strings.Cut(item, "=")
		if !ok {
			return false
		}
		switch key {
		case "max-age", "s-maxage", "stale-while-revalidate", "stale-if-error":
		default:
			return false
		}
		if _, err := strconv.ParseUint(number, 10, 63); err != nil {
			return false
		}
	}
	return value != ""
}
