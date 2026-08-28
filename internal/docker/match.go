package docker

import "strings"

// HostKind is how a compiled matcher compares hosts.
type HostKind uint8

const (
	// HostAny matches every host.
	HostAny HostKind = iota
	// HostExact is Statute native/static matching: one spelling, EqualFold.
	HostExact
	// HostTraefik is Traefik Host(): the configured spelling, plus one
	// trailing-dot fold on the rule or on the request.
	HostTraefik
)

// PathKind is how a compiled matcher compares paths.
type PathKind uint8

const (
	// PathAny matches every path ("/*").
	PathAny PathKind = iota
	// PathExact is byte equality.
	PathExact
	// PathSegment is Statute Match("/x/*"): /x, /x/, and /x/... only.
	PathSegment
	// PathByte is Traefik PathPrefix: strings.HasPrefix.
	PathByte
)

func statutePathKind(pattern string) PathKind {
	if pattern == "" || pattern == "/*" {
		return PathAny
	}
	if strings.HasSuffix(pattern, "/*") {
		return PathSegment
	}
	return PathExact
}

// CompileNative builds a statute-native matcher. Empty host is HostAny.
func CompileNative(host, path string) Matcher {
	m := Matcher{Path: path, PathKind: statutePathKind(path)}
	if host != "" {
		m.Host = host
		m.HostKind = HostExact
	}
	return m
}

func (m Matcher) matchHost(host string) bool {
	switch m.HostKind {
	case HostAny:
		return true
	case HostExact:
		return m.Host != "" && strings.EqualFold(m.Host, host)
	case HostTraefik:
		return matchTraefikHost(m.Host, host)
	default:
		return false
	}
}

func (m Matcher) matchPath(path string) bool {
	switch m.PathKind {
	case PathAny:
		return true
	case PathExact:
		return path == m.Path
	case PathSegment:
		return matchSegmentPrefix(m.Path, path)
	case PathByte:
		return strings.HasPrefix(path, m.Path)
	default:
		return false
	}
}

// Match reports whether host and path satisfy this matcher.
func (m Matcher) Match(host, path string) bool {
	return m.matchHost(host) && m.matchPath(path)
}

// matchTraefikHost mirrors pkg/muxer/http/matcher.go host() for a
// non-wildcard ASCII host: exact, then one trailing dot on the rule, then
// one trailing dot on the request. No CNAME flatten.
func matchTraefikHost(configured, reqHost string) bool {
	configured = strings.ToLower(configured)
	reqHost = strings.ToLower(reqHost)
	if reqHost == configured {
		return true
	}
	if n := len(configured); n > 0 && configured[n-1] == '.' && reqHost == configured[:n-1] {
		return true
	}
	if n := len(reqHost); n > 0 && reqHost[n-1] == '.' && reqHost[:n-1] == configured {
		return true
	}
	return false
}

func matchSegmentPrefix(pattern, path string) bool {
	before, ok := strings.CutSuffix(pattern, "/*")
	if !ok {
		return pattern == path
	}
	if before == "" {
		return true
	}
	return path == before || strings.HasPrefix(path, before+"/")
}

func traefikHostForbidden(h string) bool {
	return h == "*" || strings.Contains(h, "*")
}

func pathHasPercent(a string) bool {
	return strings.Contains(a, "%")
}

func (m Matcher) anyHost() bool {
	return m.HostKind == HostAny
}

func (m Matcher) anyPath() bool {
	return m.PathKind == PathAny || (m.PathKind == PathByte && m.Path == "/")
}

// matcherLE reports whether every request a matches is also matched by b.
func matcherLE(a, b Matcher) bool {
	return hostMatcherLE(a, b) && pathMatcherLE(a, b)
}

// hostMatcherLE reports whether every host accepted by a is accepted by b.
func hostMatcherLE(a, b Matcher) bool {
	if b.anyHost() {
		return true
	}
	if a.anyHost() {
		return false
	}
	switch b.HostKind {
	case HostExact:
		return a.HostKind == HostExact && strings.EqualFold(a.Host, b.Host)
	case HostTraefik:
		switch a.HostKind {
		case HostExact:
			return matchTraefikHost(b.Host, a.Host)
		case HostTraefik:
			return traefikHostLE(a.Host, b.Host)
		default:
			return false
		}
	default:
		return false
	}
}

// traefikHostLE reports HostTraefik(a) ⊆ HostTraefik(b). Distinct stored
// forms are not equal: Host("x.") also matches "x..", which Host("x") misses.
// Host("x") is a subset of Host("x.") because that extra request is the only
// difference.
func traefikHostLE(a, b string) bool {
	a = strings.ToLower(a)
	b = strings.ToLower(b)
	if a == b {
		return true
	}
	if !strings.HasSuffix(a, ".") && b == a+"." {
		return true
	}
	return false
}

func pathMatcherLE(a, b Matcher) bool {
	if b.anyPath() {
		return true
	}
	if a.anyPath() {
		return false
	}
	switch b.PathKind {
	case PathExact:
		return a.PathKind == PathExact && a.Path == b.Path
	case PathSegment:
		return segmentCovers(a, b.Path)
	case PathByte:
		return bytePrefixCovers(a, b.Path)
	default:
		return false
	}
}

// bytePrefixCovers reports that every path a matches starts with prefix.
func bytePrefixCovers(a Matcher, prefix string) bool {
	if prefix == "/" {
		return true
	}
	switch a.PathKind {
	case PathByte:
		return strings.HasPrefix(a.Path, prefix)
	case PathSegment:
		before, ok := strings.CutSuffix(a.Path, "/*")
		if !ok || before == "" {
			return false
		}
		return strings.HasPrefix(before, prefix)
	case PathExact:
		return strings.HasPrefix(a.Path, prefix)
	default:
		return false
	}
}

// segmentCovers reports that every path a matches is matched by statute
// pattern b (exact or trailing "/*").
func segmentCovers(a Matcher, pattern string) bool {
	bp, wildcard := strings.CutSuffix(pattern, "/*")
	if !wildcard {
		return a.PathKind == PathExact && a.Path == pattern
	}
	if bp == "" {
		return true
	}
	switch a.PathKind {
	case PathByte:
		// PathPrefix(`/api`) also matches `/api-secret`. Equality with
		// the segment base leaves that path uncovered.
		return strings.HasPrefix(a.Path, bp+"/")
	case PathSegment:
		ap, _ := strings.CutSuffix(a.Path, "/*")
		return ap == bp || strings.HasPrefix(ap, bp+"/")
	case PathExact:
		return matchSegmentPrefix(pattern, a.Path)
	default:
		return false
	}
}

func hostWitnesses(m Matcher) []string {
	if m.anyHost() || m.Host == "" {
		return []string{"", "other.example.com", "a.example.com"}
	}
	h := m.Host
	out := []string{h, strings.ToUpper(h), h + ".", h + ".."}
	if withoutDot, ok := strings.CutSuffix(h, "."); ok {
		out = append(out, withoutDot, withoutDot+".")
	}
	return out
}
