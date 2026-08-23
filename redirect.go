package statute

import (
	"fmt"
	"net/http"
	"strings"

	"statute.kjanat.dev/internal/parse"
	"statute.kjanat.dev/resolved"
)

// redirectStatuses are the status codes Route.RedirectTo accepts: the three
// classic redirects plus the method-preserving pair.
var redirectStatuses = map[int]bool{
	http.StatusMovedPermanently:  true, // 301
	http.StatusFound:             true, // 302
	http.StatusSeeOther:          true, // 303
	http.StatusTemporaryRedirect: true, // 307
	http.StatusPermanentRedirect: true, // 308
}

// The whitelisted template vocabulary. redirectPlaceholderNames is the
// order error messages list it in.
const (
	phRequestURI = "request_uri"
	phPath       = "path"
	phQuery      = "query"
	phHost       = "host"

	redirectPlaceholderNames = "{request_uri}, {path}, {query}, {host}"
)

// redirectSegment is one compiled piece of a redirect target: either a
// literal, or the name of a placeholder to substitute at request time.
type redirectSegment struct {
	literal     string
	placeholder string // empty for a literal segment
}

// parseRedirectTarget splits a target into literal and placeholder segments.
// Only "{" opens a placeholder, and its name must be one of the whitelisted
// four; a lone "}" is an ordinary character. Substituted request data is
// never rescanned, so placeholder-shaped text arriving in a path or query
// stays literal in the Location header.
func parseRedirectTarget(target string) ([]redirectSegment, error) {
	var segs []redirectSegment
	for len(target) > 0 {
		open := strings.IndexByte(target, '{')
		if open < 0 {
			segs = append(segs, redirectSegment{literal: target})
			break
		}
		if open > 0 {
			segs = append(segs, redirectSegment{literal: target[:open]})
		}
		rest := target[open:]
		clo := strings.IndexByte(rest, '}')
		if clo < 0 {
			return nil, fmt.Errorf("unclosed placeholder %q", rest)
		}
		name := rest[1:clo]
		switch name {
		case phRequestURI, phPath, phQuery, phHost:
			segs = append(segs, redirectSegment{placeholder: name})
		default:
			return nil, fmt.Errorf("unknown placeholder %q (allowed: %s)", rest[:clo+1], redirectPlaceholderNames)
		}
		target = rest[clo+1:]
	}
	return segs, nil
}

// resolveRedirect validates a route's redirect declaration. The target lands
// in the Location header, so it gets the same field-value validation as a
// configured header, plus the placeholder whitelist.
func resolveRedirect(target string, status int) (*resolved.Redirect, error) {
	if target == "" {
		return nil, fmt.Errorf("redirect_to: target is empty")
	}
	if _, err := parse.HeaderValue(target); err != nil {
		return nil, fmt.Errorf("redirect_to: %w", err)
	}
	if _, err := parseRedirectTarget(target); err != nil {
		return nil, fmt.Errorf("redirect_to: %w", err)
	}
	if !redirectStatuses[status] {
		return nil, fmt.Errorf("redirect_to: status %d is not a redirect status (allowed: 301, 302, 303, 307, 308)", status)
	}
	return &resolved.Redirect{Target: target, Status: status}, nil
}

// redirectRouteHandler answers every request with the configured redirect,
// substituting the target's placeholders from the live request. The values
// come out of net/http's request parsing — the escaped path, the raw query,
// and the validated Host — so a client cannot smuggle header-breaking bytes
// into the Location it is sent to.
func redirectRouteHandler(rd *resolved.Redirect) http.Handler {
	segs, _ := parseRedirectTarget(rd.Target) // validated at resolve time
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		for _, seg := range segs {
			switch seg.placeholder {
			case "":
				b.WriteString(seg.literal)
			case phRequestURI:
				b.WriteString(r.URL.RequestURI())
			case phPath:
				b.WriteString(r.URL.EscapedPath())
			case phQuery:
				b.WriteString(r.URL.RawQuery)
			case phHost:
				b.WriteString(stripPort(r.Host))
			}
		}
		http.Redirect(w, r, safeRedirectLocation(b.String()), rd.Status)
	})
}

// safeRedirectLocation neutralises a protocol-relative Location. A target that
// begins "//" or "/\" is a scheme-relative URL whose authority is the token
// that follows, so a client-controlled {path} or {request_uri} of
// "//evil.com" — sent as a "//evil.com" request path, or produced by a
// StripPrefix that removes the leading segment of "/api//evil.com" — would
// redirect off-site. Collapsing the leading slash run to a single "/" keeps it
// a same-origin absolute path. A target that begins with a scheme
// ("https://…"), a single "/", or anything else is already unambiguous and is
// left untouched.
func safeRedirectLocation(loc string) string {
	if len(loc) >= 2 && loc[0] == '/' && (loc[1] == '/' || loc[1] == '\\') {
		return "/" + strings.TrimLeft(loc, "/\\")
	}
	return loc
}
