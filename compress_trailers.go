package statute

import (
	"net/http"
	"strings"
)

func (c *compressResponseWriter) finishTrailers() {
	if !c.encoded {
		return
	}
	dropped := stripCompressionTrailers(c.Header())
	for name := range c.droppedTrailers {
		dropped[name] = struct{}{}
	}
	dropped.removeFrom(c.Header())
	// A forbidden late trailer must not erase this stage's own coding.
	deleteHeaderFold(c.Header(), "Content-Encoding")
	c.Header().Set("Content-Encoding", c.coding)
}

// A codec cannot forward identity metadata supplied after header commitment.
// Keep the removed announcement names until the handler has written its tail.
func stripCompressionTrailers(h http.Header) headerNameSet {
	announced, present := cacheHeaderValues(h, "Trailer")
	var keep []string
	dropped := make(headerNameSet)
	for _, line := range announced {
		for name := range strings.SplitSeq(line, ",") {
			name = strings.Trim(name, " \t")
			if compressionOwnedTrailer(name) {
				dropped.add(name)
			} else if name != "" {
				keep = append(keep, name)
			}
		}
	}
	dropped.removeFrom(h)
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
