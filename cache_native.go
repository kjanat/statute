package statute

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"golang.org/x/net/http/httpguts"
)

// cacheProxyPolicy captures propagation selection once for a native route request.
// The immutable fields and shared unsafe latch survive Retry/ETag request clones.
// It never projects or changes producer headers merely to make a cache decision.
type cacheProxyPolicy struct {
	propagator propagation.TextMapPropagator
	fields     []string
	signature  string
	unsafe     atomic.Bool
}

type cacheProxyPolicyKey struct{}

func cacheProxyPolicyFromContext(ctx context.Context) *cacheProxyPolicy {
	policy, _ := ctx.Value(cacheProxyPolicyKey{}).(*cacheProxyPolicy)
	return policy
}

func withCacheProxyPolicy(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		policy := newCacheProxyPolicy()
		ctx := context.WithValue(r.Context(), cacheProxyPolicyKey{}, policy)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func newCacheProxyPolicy() *cacheProxyPolicy {
	p := &cacheProxyPolicy{propagator: otel.GetTextMapPropagator()}
	var valid bool
	p.fields, p.signature, valid = cachePropagationFields(p.propagator)
	p.unsafe.Store(!valid || slices.ContainsFunc(p.fields, cachePropagationAffectsEligibility))
	return p
}

// Fields is part of the propagator's contract. Invalid declarations cannot prove
// selection safe. Valid field names cannot contain the signature's separator.
func cachePropagationFields(p propagation.TextMapPropagator) ([]string, string, bool) {
	fields := p.Fields()
	if !cachePropagationFieldsWithinLimits(fields) {
		return nil, "", false
	}
	fields = slices.Clone(fields)
	valid := true
	for i, field := range fields {
		if !httpguts.ValidHeaderFieldName(field) {
			valid = false
		}
		fields[i] = strings.ToLower(field)
	}
	slices.Sort(fields)
	fields = slices.Compact(fields)
	return fields, strings.Join(fields, "\x00"), valid
}

// These late injections can invalidate request eligibility even without Vary.
// Ordinary representation fields remain cacheable only when origin Vary does
// not depend on them, just as for other application-defined request fields.
func cachePropagationAffectsEligibility(name string) bool {
	switch name {
	case "authorization", "proxy-authorization", "cookie", "cache-control", "pragma",
		"if-match", "if-none-match", "if-modified-since", "if-unmodified-since", "if-range", "range",
		"connection", "proxy-connection", "keep-alive", "upgrade", "te", "trailer",
		"transfer-encoding", "content-length", "expect", phHost:
		return true
	default:
		return false
	}
}

func (p *cacheProxyPolicy) usable() bool {
	return cachePolicyUsable(p)
}

// The route-level request snapshot exists before middleware admission. Bound
// its copies independently; an oversized declaration bypasses caching while
// retaining the selected propagator and its ordinary injection side effects.
func cachePropagationFieldsWithinLimits(fields []string) bool {
	if len(fields) > cacheHeaderNames {
		return false
	}
	remaining := cacheKeyBytes
	for _, name := range fields {
		if len(name) >= remaining {
			return false
		}
		remaining -= len(name) + 1
	}
	return true
}

func (p *cacheProxyPolicy) allowsVary(names []string) bool {
	if p == nil {
		return true
	}
	return !p.unsafe.Load() && !slices.ContainsFunc(names, p.forbidsVaryField)
}

func (p *cacheProxyPolicy) forbidsVaryField(name string) bool {
	switch name {
	case "forwarded", "x-forwarded-for", "x-forwarded-host", "x-forwarded-proto":
		return true
	default:
		return slices.Contains(p.fields, name)
	}
}

func (e cacheEntry) allowedBy(p *cacheProxyPolicy) bool {
	if p == nil {
		return true
	}
	return !p.unsafe.Load() && !slices.ContainsFunc(e.vary, func(field cacheVaryField) bool {
		return p.forbidsVaryField(field.name)
	})
}

// Injection stays at the actual proxy Rewrite point, including every retry.
// OpenTelemetry's initial delegating propagator can change in place; observing
// its fields on both sides of Inject prevents storage under the old signature.
func injectProxyPropagation(r *http.Request) {
	p := cacheProxyPolicyFromContext(r.Context())
	if p == nil {
		otel.GetTextMapPropagator().Inject(r.Context(), propagation.HeaderCarrier(r.Header))
		return
	}
	p.usable()
	p.propagator.Inject(r.Context(), propagation.HeaderCarrier(r.Header))
	p.usable()
}
