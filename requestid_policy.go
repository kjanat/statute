package statute

import (
	"fmt"
	"strings"

	"statute.kjanat.dev/internal/parse"
	"statute.kjanat.dev/resolved"
)

// requestIDForbiddenOutputHeaders reserves protocol controls in both directions.
// Authorization and Cookie remain deliberate credential-writer exceptions.
// Keep the complete categorized list in docs/request-id.md in sync.
var requestIDForbiddenOutputHeaders = map[string][]string{
	"framing and connection": {
		headerContentLength, headerTransferEncoding, headerTrailer, "Connection", "Upgrade",
		"TE", "Keep-Alive", "Proxy-Connection", "Expect", "Max-Forwards",
		"Sec-WebSocket-Accept", "Sec-WebSocket-Key", "Sec-WebSocket-Version", "Sec-WebSocket-Extensions", "Sec-WebSocket-Protocol",
	},
	"representation": {
		headerContentEncoding, "Content-Type", headerContentRange, "Content-Disposition", "Content-Language", "Content-Location",
		"Accept", "Accept-Encoding", "Accept-Language", "Accept-Charset", headerAcceptRanges, "Range",
		"Content-MD5", "Digest", "Content-Digest", "Repr-Digest", "Want-Digest", "Want-Content-Digest", "Want-Repr-Digest",
	},
	"validators": {"ETag", "Last-Modified", "If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since", "If-Range"},
	"caching":    {"Cache-Control", "Vary", "Expires", "Age", "Date", "Pragma", "Warning", "CDN-Cache-Control", "Surrogate-Control"},
	"state and security": {
		"Set-Cookie", "Set-Cookie2", "Location", "Refresh", "Retry-After", "Strict-Transport-Security",
		"Content-Security-Policy", "Content-Security-Policy-Report-Only", "WWW-Authenticate", "Proxy-Authenticate", "Proxy-Authorization",
		"Authentication-Info", "Proxy-Authentication-Info", "Alt-Svc", "Alt-Used", "Clear-Site-Data",
		"X-Content-Type-Options", "X-Frame-Options", "X-XSS-Protection", "Referrer-Policy", "Permissions-Policy",
		"Cross-Origin-Opener-Policy", "Cross-Origin-Opener-Policy-Report-Only", "Cross-Origin-Embedder-Policy",
		"Cross-Origin-Embedder-Policy-Report-Only", "Cross-Origin-Resource-Policy",
		"Origin", "Access-Control-Allow-Origin", "Access-Control-Allow-Credentials", "Access-Control-Allow-Headers",
		"Access-Control-Allow-Methods", "Access-Control-Expose-Headers", "Access-Control-Max-Age", "Access-Control-Allow-Private-Network",
		"Access-Control-Request-Headers", "Access-Control-Request-Method", "Access-Control-Request-Private-Network",
		"Host", "Forwarded", headerXForwardedFor, "X-Forwarded-Host", "X-Forwarded-Proto", "X-Forwarded-Port", "X-Real-IP",
	},
}

func normalizeRequestIDOutputHeader(name string) (string, error) {
	if name == "" {
		name = defaultRequestIDHeader
	}
	name, err := parse.HeaderName(name)
	if err != nil {
		return "", fmt.Errorf("request_id output: %w", err)
	}
	for category, names := range requestIDForbiddenOutputHeaders {
		for _, reserved := range names {
			if strings.EqualFold(name, reserved) {
				return "", fmt.Errorf("request_id output %q is reserved for HTTP %s; use a tracing header", name, category)
			}
		}
	}
	return name, nil
}

// Request identity has one owner within each assembled route chain. Docker
// defaults and independently valid named chains are checked again together.
func validateRequestIDOwnership(mws []resolved.Middleware) error {
	owner := -1
	for i, mw := range mws {
		if mw.Type != resolved.MWRequestID {
			continue
		}
		if _, err := normalizeRequestIDOutputHeader(mw.RequestIDHeader); err != nil {
			return fmt.Errorf("middleware[%d]: %w", i, err)
		}
		if owner >= 0 {
			return fmt.Errorf("middleware[%d]: only one RequestID is allowed per route; already declared at middleware[%d]", i, owner)
		}
		owner = i
	}
	return nil
}
