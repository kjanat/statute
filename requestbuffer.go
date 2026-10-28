package statute

import (
	"errors"
	"fmt"
	"path/filepath"

	"statute.kjanat.dev/internal/parse"
	"statute.kjanat.dev/resolved"
)

// RequestBufferResources bounds request buffering across one server, including
// all listeners and live or retired Docker generations. The zero value permits
// memory-only buffering with a 64 MiB budget. Disk spilling requires both an
// existing operator-owned Storage directory and a positive DiskBudget.
// At most 128 buffers may be live, including readers retained after Timeout.
type RequestBufferResources struct {
	MemoryBudget string
	Storage      string
	DiskBudget   string
}

type requestBufferMW struct{ maximum, threshold string }

func (*requestBufferMW) statuteMiddleware() {}

// RequestBuffer fully consumes a bounded request body before invoking the next
// handler. Oversize returns 413; unavailable server resources return 503 with
// no-store. It never falls back to streaming and does not enable retries.
func RequestBuffer(maximum string) *requestBufferMW { return &requestBufferMW{maximum: maximum} }

// MemoryThreshold sets when buffering spills to disk, if server disk storage is
// configured. The default is min(256 KiB, maximum). Without disk storage the
// whole body is admitted against the server memory budget.
func (m *requestBufferMW) MemoryThreshold(size string) *requestBufferMW { m.threshold = size; return m }

func requestBufferSize(name, value string) (int64, error) {
	n, err := parse.Size(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	// Leave room for an oversize sentinel and an int-sized allocation.
	if n <= 0 || uint64(n) >= uint64(^uint(0)>>1) {
		return 0, fmt.Errorf("%s: size must be positive and representable with an oversize sentinel", name)
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
