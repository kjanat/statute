package statute

import (
	"net/http"
	"strings"
)

func (c *compressResponseWriter) finishTrailers() {
	if !c.encoded {
		return
	}
	c.droppedTrailers = append(c.droppedTrailers, stripCompressionTrailers(c.Header())...)
	for _, name := range c.droppedTrailers {
		deleteHeaderFold(c.Header(), name)
	}
	// A forbidden late trailer must not erase this stage's own coding.
	deleteHeaderFold(c.Header(), "Content-Encoding")
	c.Header().Set("Content-Encoding", c.coding)
}

// A codec cannot forward identity metadata supplied after header commitment.
// Keep the removed announcement names until the handler has written its tail.
func stripCompressionTrailers(h http.Header) []string {
	announced, present := cacheHeaderValues(h, "Trailer")
	var keep, dropped []string
	for _, line := range announced {
		for name := range strings.SplitSeq(line, ",") {
			name = strings.Trim(name, " \t")
			if compressionOwnedTrailer(name) {
				dropped = append(dropped, name)
				deleteHeaderFold(h, name)
			} else if name != "" {
				keep = append(keep, name)
			}
		}
	}
	if present {
		deleteHeaderFold(h, "Trailer")
		if len(keep) > 0 {
			h.Set("Trailer", strings.Join(keep, ", "))
		}
	}
	stripLateCompressionTrailers(h)
	return dropped
}

func stripLateCompressionTrailers(h http.Header) {
	for name := range h {
		if len(name) >= len(http.TrailerPrefix) && strings.EqualFold(name[:len(http.TrailerPrefix)], http.TrailerPrefix) && compressionOwnedTrailer(name[len(http.TrailerPrefix):]) {
			delete(h, name)
		}
	}
}

func compressionOwnedTrailer(name string) bool {
	switch strings.ToLower(name) {
	case "content-length", "content-encoding", "content-range", "accept-ranges", "etag", "content-md5", "digest", "content-digest", "repr-digest":
		return true
	default:
		return false
	}
}
