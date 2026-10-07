package statute

import (
	"fmt"

	"statute.kjanat.dev/resolved"
)

// A cache hit skips inner handlers. Docker chains need validation after their
// separately registered defaults and named middleware have been combined.
func validateCachePolicyOrder(mws []resolved.Middleware) error {
	cacheIndex := -1
	for i, mw := range mws {
		if mw.Type == resolved.MWCache && mw.CacheTTL > 0 && cacheIndex < 0 {
			cacheIndex = i
		}
		if cacheIndex >= 0 && (mw.Type == resolved.MWAllowIPs || mw.Type == resolved.MWDenyIPs) {
			return fmt.Errorf("middleware[%d]: IP policy must precede Cache (middleware[%d]); move AllowIPs/DenyIPs before every enabled Cache", i, cacheIndex)
		}
		if cacheIndex >= 0 && mw.Type == resolved.MWRequestID {
			return fmt.Errorf("middleware[%d]: RequestID must precede Cache (middleware[%d]); move RequestID before every enabled Cache", i, cacheIndex)
		}
	}
	return nil
}
