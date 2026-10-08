package statute

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"net/http"
	"strconv"

	"statute.kjanat.dev/internal/httpprecondition"
)

// Buffered renders have no streaming flush boundary to expose to the client.
type bufferedETagRenderKey struct{}

// etagHandler hashes a complete inner GET representation. HEAD renders that
// same path internally and omits delivery. Original preconditions are evaluated
// against the generated validator after rendering; buffering is explicit.
func etagHandler(next http.Handler) http.Handler {
	return etagHandlerWithLimit(next, defaultMaxResponseBodyBytes, defaultResponseBufferBudgetBytes)
}

func etagHandlerWithLimit(next http.Handler, limit, budgetBytes int64) http.Handler {
	budget := newResponseBufferBudget(budgetBytes)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.Method != http.MethodGet && r.Method != http.MethodHead) || r.Header.Get("Upgrade") != "" {
			next.ServeHTTP(w, r)
			return
		}
		render := r.Clone(context.WithValue(r.Context(), bufferedETagRenderKey{}, true))
		// Clone shares Body; its live trailers follow reads of that same body.
		render.Trailer = r.Trailer
		render.Method = http.MethodGet
		httpprecondition.Clear(render.Header)
		buf := newLimitedResponseBuffer(limit)
		buf.budget = budget
		defer buf.release()
		if !buf.render(next, render) {
			writeResponseLimitFailure(w, buf.failureStatus)
			return
		}
		completeETagContentType(buf)
		coding, acceptable := selectETagEncoding(r, buf)
		if !acceptable {
			buf.status = http.StatusNotAcceptable
			stripUnacceptableRepresentation(buf.header)
			buf.body.Reset()
		}
		if buf.status == http.StatusOK {
			sum := sha256.Sum256(buf.body.Bytes())
			etag := `"` + hex.EncodeToString(sum[:16]) + `"`
			if coding != "" {
				etag = "W/" + etag
			}
			setETagMetadata(buf, etag)
			if writeETagPrecondition(w, r, buf) {
				return
			}
		}
		replayETagResponse(w, r, buf)
	})
}

func writeETagPrecondition(w http.ResponseWriter, r *http.Request, buf *responseBuffer) bool {
	status := httpprecondition.Status(r.Header, buf.header)
	if status == 0 {
		return false
	}
	maps.Copy(w.Header(), buf.header)
	stripResponseTrailers(w.Header())
	deleteHeaderFold(w.Header(), "Content-Length")
	for _, name := range []string{"Content-Range", headerContentMD5, headerDigest, headerContentDigest, headerReprDigest, headerTransferEncoding, headerTrailer} {
		deleteHeaderFold(w.Header(), name)
	}
	if status != http.StatusNotModified {
		deleteHeaderFold(w.Header(), "Content-Encoding")
	}
	w.WriteHeader(status)
	return true
}

func setETagMetadata(buf *responseBuffer, etag string) {
	deleteHeaderFold(buf.header, "ETag")
	deleteHeaderFold(buf.header, "Content-Length")
	stripResponseTrailer(buf.header, "ETag")
	stripResponseTrailer(buf.header, "Content-Length")
	buf.header.Set("ETag", etag)
	if len(responseTrailerNames(buf.header)) == 0 {
		buf.header.Set("Content-Length", strconv.Itoa(buf.body.Len()))
	}
}

// An outer compressor must select the representation before read conditions
// can turn a successful render into a bodyless 304 or 412.
func selectETagEncoding(r *http.Request, buf *responseBuffer) (string, bool) {
	if n, ok := r.Context().Value(compressedRepresentationKey{}).(*compressionNegotiator); ok && buf.status == http.StatusOK {
		return n.selectCoding(buf.header, buf.status)
	}
	return "", true
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
	stripResponseTrailers(w.Header())
	if buf.status >= 200 && buf.status != http.StatusNoContent && buf.status != http.StatusNotModified {
		deleteHeaderFold(w.Header(), "Content-Length")
		w.Header().Set("Content-Length", strconv.Itoa(buf.body.Len()))
	}
	w.WriteHeader(buf.status)
}
