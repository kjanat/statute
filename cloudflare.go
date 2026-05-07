package statute

import (
	"context"
	"net/http"
)

type cfCtxKey struct{}

// behindCloudflareMiddleware tags the request context so downstream handlers
// (clientIP, rate limit, access log) know to trust Cloudflare-injected
// headers. Wrap a listener's handler chain with this only when the listener
// actually receives traffic via Cloudflare; otherwise client IPs become
// trivially spoofable by clients that send their own CF-Connecting-IP header.
func behindCloudflareMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), cfCtxKey{}, true)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func isBehindCloudflare(r *http.Request) bool {
	v, _ := r.Context().Value(cfCtxKey{}).(bool)
	return v
}
