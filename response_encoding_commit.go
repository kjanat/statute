package statute

import (
	"bufio"
	"maps"
	"net"
	"net/http"
	"strings"
	"time"
)

type committedHeaderWriter struct {
	http.ResponseWriter
	header                           http.Header
	observation                      *cacheObservation
	committed, hijacked, multiplexed bool
}

func preserveCommittedHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writer := &committedHeaderWriter{ResponseWriter: w, header: w.Header().Clone(), observation: cacheObservationFrom(r.Context()), multiplexed: r.ProtoMajor >= 2}
		next.ServeHTTP(writer, r)
		writer.finish()
	})
}

func (w *committedHeaderWriter) Header() http.Header { return w.header }

func (w *committedHeaderWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *committedHeaderWriter) publish() {
	h := w.ResponseWriter.Header()
	clear(h)
	maps.Copy(h, w.header.Clone())
}

func (w *committedHeaderWriter) commit() {
	if w.committed {
		return
	}
	w.committed = true
	w.publish()
	if w.observation != nil {
		w.observation.observe(w.ResponseWriter.Header(), time.Now())
	}
}

func (w *committedHeaderWriter) WriteHeader(status int) {
	if status == http.StatusSwitchingProtocols && w.multiplexed {
		return
	}
	if !w.committed && status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		w.publish()
	} else {
		w.commit()
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *committedHeaderWriter) Write(p []byte) (int, error) {
	w.commit()
	return w.ResponseWriter.Write(p)
}

func (w *committedHeaderWriter) Flush() {
	w.commit()
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *committedHeaderWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.hijacked = true
	}
	return conn, rw, err
}

func (w *committedHeaderWriter) finish() {
	if w.hijacked {
		return
	}
	if !w.committed {
		w.commit()
		return
	}
	h := w.ResponseWriter.Header()
	names := responseTrailerNames(h)
	names.removeFrom(h)
	for key, values := range w.header {
		if names.contains(key) || strings.HasPrefix(key, http.TrailerPrefix) {
			h[key] = values
		}
	}
}
