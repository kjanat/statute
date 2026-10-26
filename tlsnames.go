package statute

import (
	"fmt"
	"strings"
)

// tlsNameIndex implements shared TLS hostname precedence. Consumers own their
// default/fallback; an absent match must never imply a successfully selected value.
// It is populated during construction and immutable once published.
type tlsNameIndex[T any] struct {
	exact     map[string]T
	wildcards map[string]T
}

func (index *tlsNameIndex[T]) add(pattern string, value T) error {
	name := canonicalTLSName(pattern)
	if index.exact == nil {
		index.exact = make(map[string]T)
		index.wildcards = make(map[string]T)
	}
	table := index.exact
	if strings.HasPrefix(name, "*.") {
		table = index.wildcards
	}
	if _, exists := table[name]; exists {
		return fmt.Errorf("tls: hostname %q is claimed twice on this listener", name)
	}
	table[name] = value
	return nil
}

func (index *tlsNameIndex[T]) lookup(name string) (T, bool) {
	host := canonicalTLSName(name)
	if value, found := index.exact[host]; found {
		return value, true
	}
	if _, rest, ok := strings.Cut(host, "."); ok && rest != "" {
		if value, found := index.wildcards["*."+rest]; found {
			return value, true
		}
	}
	var zero T
	return zero, false
}
