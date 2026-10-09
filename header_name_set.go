package statute

import (
	"net/http"
	"strings"
	"unicode"
)

// headerNameSet deduplicates names using the same equivalence as EqualFold.
type headerNameSet map[string]struct{}

func (s headerNameSet) add(name string) { s[foldHeaderName(name)] = struct{}{} }

func (s headerNameSet) contains(name string) bool {
	_, ok := s[foldHeaderName(name)]
	return ok
}

func (s headerNameSet) removeFrom(h http.Header) {
	if len(s) == 0 {
		return
	}
	for name := range h {
		if s.contains(name) {
			delete(h, name)
		}
	}
}

func foldHeaderName(name string) string {
	if strings.IndexFunc(name, func(r rune) bool { return r > unicode.MaxASCII }) < 0 {
		return strings.ToLower(name)
	}
	// In-process handlers can construct non-ASCII keys. Preserve EqualFold's
	// simple-fold classes, including Kelvin sign and long s matching ASCII.
	var folded strings.Builder
	for _, r := range name {
		minimum := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			if next < minimum {
				minimum = next
			}
		}
		if minimum >= 'A' && minimum <= 'Z' {
			minimum += 'a' - 'A'
		}
		folded.WriteRune(minimum)
	}
	return folded.String()
}
