package statute

import (
	"net/http"
	"strings"
)

// Buffered trailer values remain in Header until replay. Keep them distinct
// from initial metadata even when the synthetic GET body is not delivered.
func responseTrailerNames(h http.Header) headerNameSet {
	names := make(headerNameSet)
	for key, values := range h {
		if strings.EqualFold(key, headerTrailer) {
			for _, value := range values {
				for name := range strings.SplitSeq(value, ",") {
					if name = strings.TrimSpace(name); name != "" {
						names.add(name)
					}
				}
			}
		}
		if strings.HasPrefix(key, http.TrailerPrefix) {
			names.add(key)
		}
	}
	return names
}

func stripResponseTrailers(h http.Header) {
	names := responseTrailerNames(h)
	names.add(headerTrailer)
	names.removeFrom(h)
}

// Remove one field from trailer announcements without changing its ordinary
// header value. The caller owns that value (for example RequestID or ETag).
func stripResponseTrailer(h http.Header, name string) {
	deleteHeaderFold(h, http.TrailerPrefix+name)
	for key, values := range h {
		if !strings.EqualFold(key, headerTrailer) {
			continue
		}
		var retained []string
		for _, value := range values {
			for token := range strings.SplitSeq(value, ",") {
				if token = strings.TrimSpace(token); token != "" && !strings.EqualFold(token, name) {
					retained = append(retained, token)
				}
			}
		}
		delete(h, key)
		if len(retained) > 0 {
			h[key] = retained
		}
	}
}
