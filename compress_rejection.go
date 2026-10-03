package statute

import (
	"net/http"
	"strings"
)

const (
	headerContentDigest    = "Content-Digest"
	headerContentMD5       = "Content-MD5"
	headerDigest           = "Digest"
	headerReprDigest       = "Repr-Digest"
	headerTransferEncoding = "Transfer-Encoding"
	headerTrailer          = "Trailer"
)

func (c *compressResponseWriter) finishRejection() {
	for _, name := range c.droppedTrailers {
		deleteHeaderFold(c.Header(), name)
	}
	stripUnacceptableRepresentation(c.Header())
}

// Rejections have an empty body; origin representation metadata and trailers
// must not describe that response. Keep unrelated policy and error headers.
func stripUnacceptableRepresentation(h http.Header) []string {
	announced, _ := cacheHeaderValues(h, headerTrailer)
	var dropped []string
	for _, line := range announced {
		for name := range strings.SplitSeq(line, ",") {
			name = strings.Trim(name, " \t")
			deleteHeaderFold(h, name)
			dropped = append(dropped, name)
		}
	}
	for name := range h {
		if len(name) >= len(http.TrailerPrefix) && strings.EqualFold(name[:len(http.TrailerPrefix)], http.TrailerPrefix) {
			delete(h, name)
		}
	}
	for _, name := range []string{"Content-Type", "Content-Length", "Content-Encoding", "Content-Language", "Content-Location", "Content-Disposition", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified", headerContentMD5, headerDigest, headerContentDigest, headerReprDigest, headerTransferEncoding, headerTrailer} {
		deleteHeaderFold(h, name)
	}
	h.Set("Content-Length", "0")
	return dropped
}
