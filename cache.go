package statute

import (
	"context"
	"net/http"

	"statute.kjanat.dev/resolved"
)

// cacheHandler owns finite storage and allocation budgets. Failure to admit a
// request or response bypasses storage without changing producer delivery.
func cacheHandler(m resolved.Middleware, next http.Handler) http.Handler {
	ttl := m.CacheTTL
	if ttl <= 0 {
		return next
	}
	c := newBoundedTTLCache(ttl, m.CacheMaxEntries, m.MaxResponseBodyBytes, m.ResponseBufferBudgetBytes)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !c.begin() {
			next.ServeHTTP(w, r)
			return
		}
		defer c.end()
		policy := cacheProxyPolicyFromContext(r.Context())
		if !cacheRequestPreflight(r, policy) || !cacheRequestEligible(r) || !cacheNativeRequestEligible(r, policy) {
			next.ServeHTTP(w, r)
			return
		}
		key := cacheKeyForRequest(r, policy)
		if entry := c.get(key, r.Header, policy); entry != nil {
			defer c.release(entry)
			if cachePolicyUsable(policy) {
				_ = cacheReplay(w, entry.buf)
				return
			}
		}
		candidate := c.candidate()
		if candidate == nil {
			next.ServeHTTP(w, r)
			return
		}
		defer c.release(candidate)
		requestHeaders := cacheCloneHeader(r.Header)
		requestAllowsStorage := cacheControlAllowsStorage(requestHeaders)
		writer := newCacheWriter(w, c, candidate)
		next.ServeHTTP(writer, r)
		writer.finish(r.Context(), key, requestHeaders, requestAllowsStorage, policy)
	})
}

func cacheStorageVary(ctx context.Context, buf *responseBuffer, requestAllowsStorage bool, policy *cacheProxyPolicy) ([]string, bool) {
	if !cacheProjectionPreflight(ctx, buf.Header()) {
		return nil, false
	}
	projected := responseHeadersForCache(ctx, buf.Header())
	vary, reusable := cacheVary(buf.Header(), projected)
	return vary, cacheResponseEligible(buf) && reusable && cachePolicyUsable(policy) &&
		requestAllowsStorage && cacheResponseAllowsStorage(buf.Header()) &&
		cacheResponseAllowsStorage(projected)
}

// Keep original and effective targets separate: Handle may inspect RequestURI
// after hoisted rewrites change URL. A typed key also avoids delimiter aliases.
type cacheKey struct {
	method, host, target, originalURI, scheme, propagationFields string
	tls                                                          bool
}

func cacheKeyForRequest(r *http.Request, policy *cacheProxyPolicy) cacheKey {
	key := cacheKey{
		method: r.Method, host: r.Host, target: r.URL.RequestURI(),
		originalURI: r.RequestURI, scheme: r.URL.Scheme, tls: r.TLS != nil,
	}
	if policy != nil {
		key.propagationFields = policy.signature
	}
	return key
}
