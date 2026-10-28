package statute

import (
	"errors"
	"fmt"
	"path/filepath"

	"statute.kjanat.dev/internal/parse"
	"statute.kjanat.dev/resolved"
)

// RequestBufferResources limits request buffering across all listeners and routes
// on one server. Readers from previous Docker configurations count toward the same
// limits. The zero value allows memory-only buffering with a 64 MiB budget. Writing
// to disk requires an existing Storage directory and a positive DiskBudget.
// At most 128 buffers may be open, including readers still running after Timeout.
type RequestBufferResources struct {
	MemoryBudget string
	Storage      string
	DiskBudget   string
}

type requestBufferMW struct{ maximum, threshold string }

func (*requestBufferMW) statuteMiddleware() {}

// RequestBuffer reads the complete request body before calling the next handler.
// Bodies larger than maximum return 413. Insufficient memory, disk space or buffer
// slots return 503 with Cache-Control: no-store, as do failed storage operations.
// Rejected requests never reach the next handler.
func RequestBuffer(maximum string) *requestBufferMW { return &requestBufferMW{maximum: maximum} }

// MemoryThreshold sets the body size above which buffering writes to disk, when
// disk storage is configured. The default is min(256 KiB, maximum). Memory-only
// buffering grows as needed up to the route maximum plus one byte. Allocations
// count against the server budget, including both buffers during growth.
func (m *requestBufferMW) MemoryThreshold(size string) *requestBufferMW { m.threshold = size; return m }

func requestBufferSize(name, value string) (int64, error) {
	n, err := parse.Size(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	// The allocation must fit in an int, including one extra byte to detect oversize.
	if n <= 0 || uint64(n) >= uint64(^uint(0)>>1) {
		return 0, fmt.Errorf("%s: size must be positive and fit in an int with one extra byte", name)
	}
	return n, nil
}

func (m *requestBufferMW) resolve() (resolved.Middleware, error) {
	n, err := requestBufferSize("request_buffer.maximum", m.maximum)
	if err != nil {
		return resolved.Middleware{}, err
	}
	threshold := min(n, int64(256<<10))
	if m.threshold != "" {
		threshold, err = requestBufferSize("request_buffer.memory_threshold", m.threshold)
	}
	if err != nil {
		return resolved.Middleware{}, err
	}
	if threshold > n {
		return resolved.Middleware{}, errors.New("request_buffer: memory threshold exceeds maximum")
	}
	return resolved.Middleware{Type: resolved.MWRequestBuffer, RequestBufferMaxBytes: n, RequestBufferMemoryThresholdBytes: threshold}, nil
}

func resolveRequestBufferResources(c RequestBufferResources) (resolved.RequestBufferResources, error) {
	out := resolved.RequestBufferResources{MemoryBudgetBytes: 64 << 20, MaxInFlight: requestBufferMaxInFlight}
	var err error
	if c.MemoryBudget != "" {
		out.MemoryBudgetBytes, err = requestBufferSize("request_buffers.memory_budget", c.MemoryBudget)
	}
	if err != nil {
		return out, err
	}
	if (c.Storage == "") != (c.DiskBudget == "") {
		return out, errors.New("request_buffers: Storage and DiskBudget must be configured together")
	}
	if c.Storage != "" {
		if !filepath.IsAbs(c.Storage) {
			return out, errors.New("request_buffers.storage: expected an absolute directory path")
		}
		out.Storage = filepath.Clean(c.Storage)
		out.DiskBudgetBytes, err = requestBufferSize("request_buffers.disk_budget", c.DiskBudget)
	}
	return out, err
}
