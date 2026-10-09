package statute

import (
	"fmt"
	"strings"

	"statute.kjanat.dev/resolved"
)

// validateBasicAuthCredentials keeps hoisted operations from supplying the
// credentials that a route's BasicAuth verifies. Docker repeats this check
// after assembling independently valid defaults and named chains.
func validateBasicAuthCredentials(mws []resolved.Middleware) error {
	lastAuth := -1
	for i, mw := range mws {
		if mw.Type == resolved.MWBasicAuth {
			lastAuth = i
		}
	}
	if lastAuth < 0 {
		return nil
	}
	inputs := map[string]bool{strings.ToLower("Authorization"): true}
	for _, mw := range mws[:lastAuth] {
		if input := basicAuthMappedInput(mw); input != "" {
			inputs[strings.ToLower(input)] = true
		}
	}
	for i, mw := range mws {
		if isRequestHeaderOp(mw.Type) && inputs[strings.ToLower(mw.HeaderName)] {
			return fmt.Errorf("middleware[%d]: request header operation on %q can replace BasicAuth credentials; remove the operation from this route", i, mw.HeaderName)
		}
	}
	return nil
}

func basicAuthMappedInput(mw resolved.Middleware) string {
	if mw.Type == resolved.MWRequestID && strings.EqualFold(mw.RequestIDHeader, "Authorization") {
		return mw.RequestIDFromHeader
	}
	return ""
}
