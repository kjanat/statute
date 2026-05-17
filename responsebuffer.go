package statute

import (
	"bytes"
	"maps"
	"net/http"
)

// responseBuffer captures status, headers, and body so middleware can inspect
// the response before committing it to the wire. It implements http.Flusher
// as a no-op because we deliberately defer flushing until replay; streaming
// responses bypass middleware that buffers.
type responseBuffer struct {
	header      http.Header
	status      int
	body        bytes.Buffer
	wroteHeader bool
}

func newResponseBuffer() *responseBuffer {
	return &responseBuffer{header: make(http.Header), status: http.StatusOK}
}

func (b *responseBuffer) Header() http.Header { return b.header }

func (b *responseBuffer) WriteHeader(code int) {
	if b.wroteHeader {
		return
	}
	b.status = code
	b.wroteHeader = true
}

func (b *responseBuffer) Write(p []byte) (int, error) {
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
	maps.Copy(w.Header(), b.header)
	w.WriteHeader(b.status)
	_, _ = w.Write(b.body.Bytes())
}
