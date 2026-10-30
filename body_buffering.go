package statute

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/vulcand/oxy/v2/buffer"

	"statute.kjanat.dev/resolved"
)

const (
	defaultBodyBufferMaxBytes    = 8 << 20
	defaultBodyBufferMaxInFlight = 128
)

// BodyBufferPolicy sets request and response limits, memory spill thresholds
// and concurrent buffered requests. Zero maximums select 8 MiB and zero
// MaxInFlight selects 128. Excess data spills to temporary files owned by the
// request. RetryExpression uses Attempts, ResponseCode and IsNetworkError.
type BodyBufferPolicy = resolved.BodyBufferPolicy

type bufferBodiesMW struct{ policy BodyBufferPolicy }

func (*bufferBodiesMW) statuteMiddleware() {}

// BufferBodies buffers requests and responses with per-request disk spill.
// Native RequestBuffer keeps its existing server-wide budgets and defaults.
func BufferBodies(policy BodyBufferPolicy) *bufferBodiesMW { return &bufferBodiesMW{policy: policy} }

func bodyBufferOptions(p BodyBufferPolicy) []buffer.Option {
	options := []buffer.Option{
		buffer.MaxRequestBodyBytes(p.MaxRequestBytes), buffer.MemRequestBodyBytes(p.MemoryRequestBytes),
		buffer.MaxResponseBodyBytes(p.MaxResponseBytes), buffer.MemResponseBodyBytes(p.MemoryResponseBytes),
	}
	if p.RetryExpression != "" {
		options = append(options, buffer.Retry(p.RetryExpression))
	}
	if p.DisableRequest {
		options = append(options, buffer.DisableRequestBuffer())
	}
	if p.DisableResponse {
		options = append(options, buffer.DisableResponseBuffer())
	}
	return options
}

func (m *bufferBodiesMW) resolve() (resolved.Middleware, error) {
	policy := m.policy
	if policy.MaxRequestBytes == 0 {
		policy.MaxRequestBytes = defaultBodyBufferMaxBytes
	}
	if policy.MaxResponseBytes == 0 {
		policy.MaxResponseBytes = defaultBodyBufferMaxBytes
	}
	if policy.MaxInFlight < 0 {
		return resolved.Middleware{}, errors.New("buffer_bodies: max in-flight must be nonnegative")
	}
	if policy.MaxInFlight == 0 {
		policy.MaxInFlight = defaultBodyBufferMaxInFlight
	}
	if _, err := buffer.New(http.NotFoundHandler(), bodyBufferOptions(policy)...); err != nil {
		return resolved.Middleware{}, fmt.Errorf("buffer_bodies: %w", err)
	}
	return resolved.Middleware{Type: resolved.MWBufferBodies, BodyBuffers: &policy}, nil
}

func bufferBodiesHandler(m resolved.Middleware, next http.Handler) http.Handler {
	if m.BodyBuffers == nil || m.BodyBuffers.MaxInFlight <= 0 {
		return invalidBodyBufferHandler()
	}
	policy := *m.BodyBuffers
	handler, err := buffer.New(bodyBufferNext(policy, next), bodyBufferOptions(policy)...)
	if err != nil {
		return invalidBodyBufferHandler()
	}
	if policy.DisableRequest && policy.DisableResponse {
		return handler
	}
	slots := make(chan struct{}, policy.MaxInFlight)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case slots <- struct{}{}:
		default:
			writeResponseLimitFailure(w, http.StatusServiceUnavailable)
			return
		}
		defer func() { <-slots }()
		handler.ServeHTTP(w, r)
	})
}

func invalidBodyBufferHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "invalid body buffer policy", http.StatusInternalServerError)
	})
}

func bodyBufferNext(policy BodyBufferPolicy, next http.Handler) http.Handler {
	if policy.DisableResponse {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Oxy stores the last explicit status and requires an initial value.
		w.WriteHeader(http.StatusOK)
		observeCacheCommit(next, w, r, false)
	})
}
