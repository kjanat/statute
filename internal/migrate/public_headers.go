package migrate

import (
	"strconv"
	"strings"
)

// These standard policy values contain no credential material. Arbitrary header
// values still require environment bindings instead of being copied into Go.
func publicHeaderValue(name, value string) bool {
	switch strings.ToLower(name) {
	case "x-frame-options":
		return value == "DENY" || value == "SAMEORIGIN"
	case "x-xss-protection":
		return value == "0" || value == "1" || value == "1; mode=block"
	case "x-forwarded-proto":
		return value == "http" || value == "https" || value == "ws" || value == "wss"
	case "x-robots-tag":
		for item := range strings.SplitSeq(value, ",") {
			switch strings.TrimSpace(strings.ToLower(item)) {
			case "all", "none", "noindex", "index", "nofollow", "follow", "noarchive", "nosnippet", "noimageindex", "notranslate", "nocache":
			default:
				return false
			}
		}
		return value != ""
	case "cache-control":
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
	return false
}
