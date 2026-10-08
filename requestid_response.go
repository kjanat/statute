package statute

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"slices"
)

// Request identity retains the effective committed value across late producer
// writes. Buffered responses share a mutable header map until replay.
type requestIDResponseWriter struct {
	*headerResponseWriter
	name     string
	values   []string
	captured bool
	hijacked bool
}

func (w *requestIDResponseWriter) capture() {
	if !w.captured {
		w.captured = true
		values, _ := cacheHeaderValues(w.Header(), w.name)
		w.values = slices.Clone(values)
	}
}

func (w *requestIDResponseWriter) WriteHeader(status int) {
	w.headerResponseWriter.WriteHeader(status)
	if status >= 200 {
		w.capture()
	}
}

func (w *requestIDResponseWriter) Write(b []byte) (int, error) {
	n, err := w.headerResponseWriter.Write(b)
	w.capture()
	return n, err
}

func (w *requestIDResponseWriter) ReadFrom(r io.Reader) (int64, error) {
	// A reader can publish trailers while copying. Commit and snapshot before
	// calling it, keeping the underlying ReaderFrom optimization available.
	if !w.captured {
		w.WriteHeader(http.StatusOK)
	}
	return w.headerResponseWriter.ReadFrom(r)
}

func (w *requestIDResponseWriter) Flush() {
	w.headerResponseWriter.Flush()
	w.capture()
}

func (w *requestIDResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := w.headerResponseWriter.Hijack()
	if err == nil {
		w.hijacked = true
	}
	return conn, rw, err
}

// Finish runs only on normal return. The snapshot includes explicit outer header
// operations already committed; pending outer operations still run at replay.
func (w *requestIDResponseWriter) finish() {
	if w.hijacked {
		return
	}
	w.applyOps()
	w.capture()
	h := w.Header()
	stripResponseTrailer(h, w.name)
	deleteHeaderFold(h, w.name)
	if w.values != nil {
		h[w.name] = slices.Clone(w.values)
	}
}
