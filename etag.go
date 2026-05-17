package statute

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"net/http"
)

// etagHandler computes an ETag (sha256 of the response body) for 200 GET/HEAD
// responses and answers 304 Not Modified when the request's If-None-Match
// matches. It is intended for static-file routes; for proxy routes, the
// upstream usually owns ETag generation.
func etagHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		buf := newResponseBuffer()
		next.ServeHTTP(buf, r)
		if buf.status == http.StatusOK {
			sum := sha256.Sum256(buf.body.Bytes())
			etag := `"` + hex.EncodeToString(sum[:16]) + `"`
			buf.header.Set("ETag", etag)
			if r.Header.Get("If-None-Match") == etag {
				maps.Copy(w.Header(), buf.header)
				w.Header().Del("Content-Length")
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		buf.replay(w)
	})
}
