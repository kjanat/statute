package statute

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"statute.kjanat.dev/resolved"
)

type requestPathMW struct {
	kind                 resolved.MiddlewareType
	pattern, replacement string
	prefixes             []string
	record, forceSlash   bool
}

func (*requestPathMW) statuteMiddleware() {}

// ReplaceEscapedPath replaces the escaped path without changing the query.
// It executes at its declared middleware position.
func ReplaceEscapedPath(path string) *requestPathMW {
	return &requestPathMW{kind: resolved.MWReplaceEscapedPath, replacement: path}
}

// RewriteEscapedPath applies a regular expression to the escaped request path.
// A changed path must remain normalized; an unsafe result returns 400.
func RewriteEscapedPath(pattern, replacement string) *requestPathMW {
	return &requestPathMW{kind: resolved.MWRewriteEscapedPath, pattern: strings.TrimSpace(pattern), replacement: strings.TrimSpace(replacement)}
}

// StripBytePrefixes removes the first matching decoded prefix, including a
// prefix ending inside a path segment. Prefixes are checked in declaration order.
func StripBytePrefixes(prefixes ...string) *requestPathMW {
	values := make([]string, len(prefixes))
	for i, prefix := range prefixes {
		values[i] = strings.TrimSpace(prefix)
	}
	return &requestPathMW{kind: resolved.MWStripBytePrefixes, prefixes: values}
}

// PrefixPath adds a literal prefix at its declared middleware position.
func PrefixPath(prefix string) *requestPathMW {
	return &requestPathMW{kind: resolved.MWPrefixPath, replacement: prefix}
}

// RecordOriginal appends the original escaped path to X-Replaced-Path, or the
// removed prefix to X-Forwarded-Prefix when used with StripBytePrefixes.
func (m *requestPathMW) RecordOriginal() *requestPathMW { m.record = true; return m }

// ForceSlash keeps a slash when stripping the complete path.
func (m *requestPathMW) ForceSlash() *requestPathMW { m.forceSlash = true; return m }

func (m *requestPathMW) resolve() (resolved.Middleware, error) {
	if strings.ContainsAny(m.pattern+m.replacement+strings.Join(m.prefixes, ""), "\r\n\x00") {
		return resolved.Middleware{}, errors.New("request_path: invalid control character")
	}
	if m.kind == resolved.MWRewriteEscapedPath {
		if _, err := regexp.Compile(m.pattern); err != nil {
			return resolved.Middleware{}, fmt.Errorf("rewrite_escaped_path: %w", err)
		}
	}
	if m.kind == resolved.MWReplaceEscapedPath {
		if _, err := url.PathUnescape(m.replacement); err != nil {
			return resolved.Middleware{}, errors.New("replace_escaped_path: invalid escape")
		}
	}
	if m.kind == resolved.MWPrefixPath && m.replacement == "" {
		return resolved.Middleware{}, errors.New("prefix_path: prefix is empty")
	}
	return resolved.Middleware{Type: m.kind, PathPattern: m.pattern, PathReplacement: m.replacement, PathPrefixes: append([]string(nil), m.prefixes...), PathRecordOriginal: m.record, PathForceSlash: m.forceSlash}, nil
}

func requestPathHandler(m resolved.Middleware, next http.Handler) http.Handler {
	var expression *regexp.Regexp
	if m.Type == resolved.MWRewriteEscapedPath {
		var err error
		expression, err = regexp.Compile(m.PathPattern)
		if err != nil {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "invalid path policy", http.StatusInternalServerError)
			})
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out := r.Clone(r.Context())
		out.Trailer = r.Trailer
		current := out.URL.RawPath
		if current == "" {
			current = out.URL.EscapedPath()
		}
		if expression != nil && !expression.MatchString(current) {
			next.ServeHTTP(w, r)
			return
		}
		normalize, err := applyRequestPath(m, expression, out, current)
		if err != nil {
			status := http.StatusInternalServerError
			if m.Type == resolved.MWStripBytePrefixes {
				status = http.StatusBadRequest
			}
			http.Error(w, err.Error(), status)
			return
		}
		out.RequestURI = out.URL.RequestURI()
		if normalize && !requestPathNormalized(out) {
			http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, out)
	})
}

func applyRequestPath(m resolved.Middleware, expression *regexp.Regexp, out *http.Request, current string) (bool, error) {
	switch m.Type {
	case resolved.MWReplaceEscapedPath, resolved.MWRewriteEscapedPath:
		if m.PathRecordOriginal {
			out.Header.Add("X-Replaced-Path", current)
		}
		raw := m.PathReplacement
		if expression != nil {
			raw = expression.ReplaceAllString(current, raw)
		}
		path, err := url.PathUnescape(raw)
		if err != nil {
			return false, errors.New("invalid rewritten path")
		}
		out.URL.RawPath, out.URL.Path = raw, path
		return expression != nil, nil
	case resolved.MWPrefixPath:
		addPathPrefix(out.URL, m.PathReplacement)
		out.URL.Path = rootRequestPath(out.URL.Path, false)
		if out.URL.RawPath != "" {
			out.URL.RawPath = rootRequestPath(out.URL.RawPath, false)
		}
	case resolved.MWStripBytePrefixes:
		return stripRequestPrefixes(m, out)
	default:
	}
	return false, nil
}

func stripRequestPrefixes(m resolved.Middleware, out *http.Request) (bool, error) {
	for _, prefix := range m.PathPrefixes {
		if !strings.HasPrefix(out.URL.Path, prefix) {
			continue
		}
		out.URL.Path = rootRequestPath(strings.TrimPrefix(out.URL.Path, prefix), m.PathForceSlash)
		if out.URL.RawPath != "" {
			tail, ok := rawTailFromDecodedOffset(out.URL.RawPath, len(prefix))
			if !ok {
				return false, errors.New("invalid escaped path")
			}
			out.URL.RawPath = rootRequestPath(tail, m.PathForceSlash)
		}
		if m.PathRecordOriginal {
			out.Header.Add("X-Forwarded-Prefix", prefix)
		}
		return true, nil
	}
	return false, nil
}

func requestPathNormalized(out *http.Request) bool {
	if out.URL.Path == "" {
		return true
	}
	path := out.URL.Path
	out.URL = out.URL.JoinPath()
	return path == out.URL.Path
}

func rootRequestPath(path string, force bool) string {
	if force {
		return "/" + strings.TrimPrefix(path, "/")
	}
	if path != "" && path[0] != '/' {
		return "/" + path
	}
	return path
}
