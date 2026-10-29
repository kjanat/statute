package statute

import (
	"fmt"
	"net/http"

	"github.com/vulcand/oxy/v2/buffer"

	"statute.kjanat.dev/resolved"
)

// BodyBufferPolicy sets request and response limits and memory spill thresholds.
// Zero limits permit any body size. Excess data spills to temporary files owned
// by the request. RetryExpression uses Attempts, ResponseCode and IsNetworkError.
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
	if _, err := buffer.New(http.NotFoundHandler(), bodyBufferOptions(m.policy)...); err != nil {
		return resolved.Middleware{}, fmt.Errorf("buffer_bodies: %w", err)
	}
	policy := m.policy
	return resolved.Middleware{Type: resolved.MWBufferBodies, BodyBuffers: &policy}, nil
}

func bufferBodiesHandler(m resolved.Middleware, next http.Handler) http.Handler {
	if m.BodyBuffers != nil {
		if handler, err := buffer.New(next, bodyBufferOptions(*m.BodyBuffers)...); err == nil {
			return handler
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "invalid body buffer policy", http.StatusInternalServerError)
	})
}
