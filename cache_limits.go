package statute

import (
	"context"
	"net/http"
	"strings"

	"golang.org/x/net/http/httpguts"
)

const (
	cacheScratchBytes      int64 = 512 << 10
	cacheEntryControlBytes int64 = 512
	cacheKeyBytes                = 16 << 10
	cacheHeaderBytes             = 64 << 10
	cacheHeaderNames             = 256
	cacheHeaderValueLimit        = 1024
	cacheVaryTokenLimit          = 256
)

// Costs include conservative map buckets, slice descriptors/backing storage,
// and strings. Scratch covers six header worksets, two keys and control state,
// with a further 64 KiB margin. These scans do not allocate.
func cacheHeaderCost(h http.Header) (int64, bool) {
	if len(h) > cacheHeaderNames {
		return 0, false
	}
	cost, values := int64(256), 0
	for name, vv := range h {
		if len(name) > cacheHeaderBytes || len(vv) > cacheHeaderValueLimit-values {
			return 0, false
		}
		values += len(vv)
		cost += 128 + int64(len(name)) + int64(len(vv))*32
		if cost > cacheHeaderBytes {
			return 0, false
		}
		for _, value := range vv {
			if int64(len(value)) > cacheHeaderBytes-cost {
				return 0, false
			}
			cost += int64(len(value))
		}
	}
	if _, ok := cacheVaryTokenCount(h, 0); !ok {
		return 0, false
	}
	trailerTokens, ok := cacheFieldTokenCount(h, "Trailer", 0)
	// Projection can expand Trailer into individual values while removing
	// a route-owned identity trailer; include that workset in the ceiling.
	cost += int64(trailerTokens) * 32
	return cost, ok && cost <= cacheHeaderBytes
}

func cacheRequestPreflight(r *http.Request, p *cacheProxyPolicy) bool {
	remaining := cacheRequestKeyRemaining(r)
	if remaining < 0 {
		return false
	}
	if p != nil && (len(p.signature) > remaining || len(p.fields) > cacheHeaderNames) {
		return false
	}
	_, ok := cacheHeaderCost(r.Header)
	return ok
}

func cacheRequestKeyRemaining(r *http.Request) int {
	if r.URL == nil {
		return -1
	}
	remaining := cacheKeyBytes
	for _, s := range []string{r.Method, r.Host, r.RequestURI, r.URL.Scheme, r.URL.RawQuery, r.URL.RawPath, r.URL.Opaque} {
		if len(s) > remaining {
			return -1
		}
		remaining -= len(s)
	}
	if strings.HasPrefix(r.URL.Opaque, "//") {
		if len(r.URL.Scheme) > remaining {
			return -1
		}
		remaining -= len(r.URL.Scheme)
	}
	// EscapedPath can expand each byte threefold. Check before RequestURI (or
	// RawPath validation) is allowed to allocate.
	if remaining < 4 || len(r.URL.Path) > (remaining-4)/3 {
		return -1
	}
	return remaining - 4 - 3*len(r.URL.Path)
}

func cacheProjectionPreflight(ctx context.Context, h http.Header) bool {
	cost, ok := cacheHeaderCost(h)
	if !ok {
		return false
	}
	ops, _ := ctx.Value(responseHeaderOpsKey{}).([]headerOp)
	if len(ops) > cacheHeaderNames-len(h) {
		return false
	}
	a := cacheProjectionCost{cost: cost}
	for _, vv := range h {
		a.values += len(vv)
	}
	a.varyTokens, _ = cacheVaryTokenCount(h, 0)
	a.trailerTokens, _ = cacheFieldTokenCount(h, "Trailer", 0)
	for _, op := range ops {
		if !a.add(op) {
			return false
		}
	}
	return true
}

type cacheProjectionCost struct {
	cost                              int64
	values, varyTokens, trailerTokens int
}

func (a *cacheProjectionCost) add(op headerOp) bool {
	if len(op.name) > cacheHeaderBytes || len(op.value) > cacheHeaderBytes {
		return false
	}
	a.cost += 160 + int64(len(op.name)) + int64(len(op.value))
	a.values++
	if op.ensureVary || strings.EqualFold(op.name, "Vary") {
		a.varyTokens += 1 + strings.Count(op.value, ",")
	}
	if strings.EqualFold(op.name, "Trailer") {
		n := 1 + strings.Count(op.value, ",")
		a.trailerTokens += n
		a.cost += int64(n) * 32
		a.values += n
	}
	return a.cost <= cacheHeaderBytes && a.values <= cacheHeaderValueLimit && a.varyTokens <= cacheVaryTokenLimit && a.trailerTokens <= cacheVaryTokenLimit
}

func cacheVaryTokenCount(h http.Header, tokens int) (int, bool) {
	return cacheFieldTokenCount(h, "Vary", tokens)
}

func cacheFieldTokenCount(h http.Header, field string, tokens int) (int, bool) {
	for name, values := range h {
		if !strings.EqualFold(name, field) {
			continue
		}
		for _, value := range values {
			tokens += 1 + strings.Count(value, ",")
			if tokens > cacheVaryTokenLimit {
				return tokens, false
			}
		}
	}
	return tokens, true
}

func cacheCloneHeader(h http.Header) http.Header {
	clone := make(http.Header, len(h))
	for name, values := range h {
		var vv []string
		if values != nil {
			vv = make([]string, len(values))
			for i, v := range values {
				vv[i] = strings.Clone(v)
			}
		}
		clone[strings.Clone(name)] = vv
	}
	return clone
}

func cacheCloneKey(k cacheKey) cacheKey {
	return cacheKey{method: strings.Clone(k.method), host: strings.Clone(k.host), target: strings.Clone(k.target), originalURI: strings.Clone(k.originalURI), scheme: strings.Clone(k.scheme), propagationFields: strings.Clone(k.propagationFields), tls: k.tls}
}

func cacheMetadataCost(key cacheKey, request http.Header, names []string, response http.Header) (int64, bool) {
	keySize := 0
	for _, s := range []string{key.method, key.host, key.target, key.originalURI, key.scheme, key.propagationFields} {
		if len(s) > cacheKeyBytes-keySize {
			return 0, false
		}
		keySize += len(s)
	}
	rq, ok := cacheHeaderCost(request)
	if !ok {
		return 0, false
	}
	rs, ok := cacheHeaderCost(response)
	if !ok || len(names) > cacheVaryTokenLimit {
		return 0, false
	}
	cost := int64(keySize) + rq + rs
	for _, name := range names {
		if len(name) > cacheHeaderBytes {
			return 0, false
		}
		cost += 64 + int64(len(name))
	}
	return cost, true
}

// Rechecks never copy a Fields result. Fields itself belongs to the propagator.
func cachePolicyUsable(p *cacheProxyPolicy) bool {
	if p == nil {
		return true
	}
	if p.unsafe.Load() {
		return false
	}
	fields := p.propagator.Fields()
	if !cachePropagationFieldsWithinLimits(fields) || !cacheFieldsEqual(p.fields, fields) {
		p.unsafe.Store(true)
		return false
	}
	return !p.unsafe.Load()
}

func cacheFieldsEqual(expected, fields []string) bool {
	for _, name := range fields {
		if !httpguts.ValidHeaderFieldName(name) || !cacheContainsField(expected, name) {
			return false
		}
	}
	for _, name := range expected {
		if !cacheContainsField(fields, name) {
			return false
		}
	}
	return true
}

func cacheContainsField(fields []string, name string) bool {
	for _, field := range fields {
		if strings.EqualFold(field, name) {
			return true
		}
	}
	return false
}

func cacheNativeRequestEligible(r *http.Request, p *cacheProxyPolicy) bool {
	if p == nil {
		return true
	}
	for name := range r.Header {
		if strings.EqualFold(name, "Connection") {
			return false
		}
	}
	return cachePolicyUsable(p)
}
