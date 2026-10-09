package statute

import (
	"bytes"
	"context"
	"maps"
	"net/http"
	"strings"
)

// responseBuffer captures status, headers, and body so middleware can inspect
// the response before committing it to the wire. It implements http.Flusher
// as a no-op. All body bytes are buffered until replay.
type responseBuffer struct {
	header        http.Header
	status        int
	body          bytes.Buffer
	wroteHeader   bool
	multiplexed   bool
	maxBodyBytes  int64
	overLimit     bool
	failureStatus int
	budget        *responseBufferBudget
	cancel        context.CancelFunc
}

func newResponseBuffer() *responseBuffer {
	return &responseBuffer{header: make(http.Header), status: http.StatusOK}
}

// Header returns the buffered response header map.
func (b *responseBuffer) Header() http.Header { return b.header }

// WriteHeader records the status code once; later calls are ignored.
func (b *responseBuffer) WriteHeader(code int) {
	if code == http.StatusSwitchingProtocols && b.multiplexed {
		return
	}
	if code < 200 && code != http.StatusSwitchingProtocols {
		return
	}
	if b.wroteHeader {
		return
	}
	b.status = code
	b.wroteHeader = true
}

// Write appends to the buffered body, defaulting the status to 200.
func (b *responseBuffer) Write(p []byte) (int, error) {
	if b.maxBodyBytes > 0 && !b.prepareBodyWrite(len(p)) {
		return 0, errResponseBufferLimit
	}
	if !b.wroteHeader {
		b.wroteHeader = true
	}
	return b.body.Write(p)
}

// Flush is a no-op so handlers that flush mid-response do not crash; the
// buffered output is replayed by replay() after the handler returns.
func (b *responseBuffer) Flush() {}

// replay copies the buffered response into the real ResponseWriter.
func (b *responseBuffer) replay(w http.ResponseWriter) {
	maps.Copy(w.Header(), b.header.Clone())
	trailers := make(http.Header)
	names := responseTrailerNames(b.header)
	if len(names) > 0 {
		for key, values := range w.Header() {
			if names.contains(key) {
				trailers[key] = values
				delete(w.Header(), key)
			}
		}
	}
	// Late trailers must be announced before commit so HTTP/1.1 chooses
	// chunked framing even for small bodies.
	for name := range trailers {
		if strings.HasPrefix(name, http.TrailerPrefix) {
			w.Header()[name] = nil
		}
	}
	w.WriteHeader(b.status)
	_, _ = w.Write(b.body.Bytes())
	maps.Copy(w.Header(), trailers)
}
