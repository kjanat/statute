package statute

import (
	"fmt"

	"statute.kjanat.dev/resolved"
)

// Request identity has one owner within each assembled route chain. Docker
// defaults and independently valid named chains are checked again together.
func validateRequestIDOwnership(mws []resolved.Middleware) error {
	owner := -1
	for i, mw := range mws {
		if mw.Type != resolved.MWRequestID {
			continue
		}
		if owner >= 0 {
			return fmt.Errorf("middleware[%d]: only one RequestID is allowed per route; already declared at middleware[%d]", i, owner)
		}
		owner = i
	}
	return nil
}
