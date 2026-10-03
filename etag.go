package statute

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"net/http"
	"strconv"

	"statute.kjanat.dev/internal/httpprecondition"
)

// etagHandler hashes a complete inner GET representation. HEAD renders that
// same path internally and omits delivery. Original preconditions are evaluated
// against the generated validator after rendering; buffering is explicit.
func etagHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.Method != http.MethodGet && r.Method != http.MethodHead) || r.Header.Get("Upgrade") != "" {
			next.ServeHTTP(w, r)
			return
		}
		render := r.Clone(r.Context())
		render.Method = http.MethodGet
		httpprecondition.Clear(render.Header)
		buf := newResponseBuffer()
		next.ServeHTTP(buf, render)
		completeETagContentType(buf)
		if buf.status == http.StatusOK {
			sum := sha256.Sum256(buf.body.Bytes())
			etag := `"` + hex.EncodeToString(sum[:16]) + `"`
			if encoded, _ := r.Context().Value(compressedRepresentationKey{}).(bool); encoded {
				etag = "W/" + etag
			}
			deleteHeaderFold(buf.header, "ETag")
			deleteHeaderFold(buf.header, "Content-Length")
			buf.header.Set("ETag", etag)
			buf.header.Set("Content-Length", strconv.Itoa(buf.body.Len()))
			if status := httpprecondition.Status(r.Header, buf.header); status != 0 {
				maps.Copy(w.Header(), buf.header)
				deleteHeaderFold(w.Header(), "Content-Length")
				for _, name := range []string{"Content-Range", "Content-MD5", "Digest", "Content-Digest", "Repr-Digest", "Transfer-Encoding", "Trailer"} {
					deleteHeaderFold(w.Header(), name)
				}
				if status != http.StatusNotModified {
					deleteHeaderFold(w.Header(), "Content-Encoding")
				}
				w.WriteHeader(status)
				return
			}
		}
		replayETagResponse(w, r, buf)
	})
}

// Infer a missing representation type before suppressing HEAD delivery.
func completeETagContentType(buf *responseBuffer) {
	_, typed := cacheHeaderValues(buf.header, "Content-Type")
	_, encoded := cacheHeaderValues(buf.header, "Content-Encoding")
	if !typed && !encoded && buf.body.Len() > 0 {
		buf.header.Set("Content-Type", http.DetectContentType(buf.body.Bytes()))
	}
}

func replayETagResponse(w http.ResponseWriter, r *http.Request, buf *responseBuffer) {
	if r.Method != http.MethodHead {
		buf.replay(w)
		return
	}
	maps.Copy(w.Header(), buf.header)
	if buf.status >= 200 && buf.status != http.StatusNoContent && buf.status != http.StatusNotModified {
		deleteHeaderFold(w.Header(), "Content-Length")
		w.Header().Set("Content-Length", strconv.Itoa(buf.body.Len()))
	}
	w.WriteHeader(buf.status)
}
