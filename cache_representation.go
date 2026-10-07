package statute

import (
	"net/http"
	"slices"
	"strings"

	"golang.org/x/net/http/httpguts"
)

// This TTL cache delegates preconditions, ranges, and request-specific policy
// to the downstream producer. Such requests neither use nor replace entries.
func cacheRequestEligible(r *http.Request) bool {
	if r.TLS != nil && (len(r.TLS.PeerCertificates) != 0 || len(r.TLS.VerifiedChains) != 0) {
		return false
	}
	if (r.Method != http.MethodGet && r.Method != http.MethodHead) || r.Header.Get("Upgrade") != "" {
		return false
	}
	for name := range r.Header {
		switch strings.ToLower(name) {
		case "authorization", "cookie", "if-match", "if-none-match", "if-modified-since", "if-unmodified-since", "range", "if-range":
			return false
		}
	}
	return cacheControlAllows(r.Header, "no-transform", "no-cache")
}

// Origin privacy restrictions survive hoisted response-header removal. Cookie
// issuance is never shared, even when the response otherwise permits caching.
func cacheResponseAllowsStorage(h http.Header) bool {
	_, setsCookie := cacheHeaderValues(h, "Set-Cookie")
	return !setsCookie && cacheControlAllows(h, "no-store", "private", "no-cache")
}

func cacheResponseEligible(buf *responseBuffer) bool {
	_, partial := cacheHeaderValues(buf.header, "Content-Range")
	return buf.status >= 200 && buf.status < 300 && buf.status != http.StatusPartialContent && !partial
}

// The union prevents hoisted header operations from erasing origin variance.
// Invalid Vary and wildcard variance cannot identify a reusable representation.
func cacheVary(headers ...http.Header) ([]string, bool) {
	var names []string
	for _, h := range headers {
		values, _ := cacheHeaderValues(h, "Vary")
		for _, value := range values {
			for field := range strings.SplitSeq(value, ",") {
				name := strings.ToLower(strings.Trim(field, " \t"))
				if name == "" {
					continue
				}
				if name == "*" || !httpguts.ValidHeaderFieldName(name) {
					return nil, false
				}
				names = append(names, name)
			}
		}
	}
	slices.Sort(names)
	return slices.Compact(names), true
}

// Copy all case-insensitive fields in stable key order, including custom
// handlers' noncanonical keys. Presence distinguishes absence from empty.
func cacheHeaderValues(h http.Header, name string) ([]string, bool) {
	var keys []string
	for key := range h {
		if strings.EqualFold(key, name) {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	var values []string
	for _, key := range keys {
		values = append(values, h[key]...)
	}
	return values, len(keys) != 0
}

type cacheVaryField struct {
	name    string
	values  []string
	present bool
}

func (e cacheEntry) matches(h http.Header) bool {
	for _, field := range e.vary {
		values, present := cacheHeaderValues(h, field.name)
		if present != field.present || !slices.Equal(values, field.values) {
			return false
		}
	}
	return true
}

func (e cacheEntry) sameVary(other cacheEntry) bool {
	return slices.EqualFunc(e.vary, other.vary, func(a, b cacheVaryField) bool { return a.name == b.name })
}
