package statute

import (
	"io"
	"os"
)

// Observability bundles the logging, metrics, and tracing configuration.
type Observability struct {
	AccessLog AccessLog
	Metrics   Metrics
}

// AccessLog is a marker for an access log destination.
type AccessLog interface {
	statuteAccessLog()
}

// Metrics is a marker for a metrics endpoint configuration.
type Metrics interface {
	statuteMetrics()
}

// LogWriter identifies a destination for structured logs.
type LogWriter struct {
	w    io.Writer
	name string
}

// Stdout writes logs to process stdout.
var Stdout = LogWriter{w: os.Stdout, name: "stdout"}

// Stderr writes logs to process stderr.
var Stderr = LogWriter{w: os.Stderr, name: "stderr"}

// Writer returns the underlying io.Writer. Useful when constructing the
// resolved configuration.
func (l LogWriter) Writer() io.Writer { return l.w }

// Name returns a human-readable name for the destination.
func (l LogWriter) Name() string { return l.name }

type jsonLog struct{ dest LogWriter }

func (jsonLog) statuteAccessLog() {}

// JSONLog returns a structured (JSON) access log writing to the given destination.
func JSONLog(dest LogWriter) AccessLog { return jsonLog{dest: dest} }

type prometheusMetrics struct {
	addr string
	path string
}

func (prometheusMetrics) statuteMetrics() {}

// Prometheus exposes process and request metrics on the given address and
// path, formatted in the Prometheus exposition format.
func Prometheus(addr, path string) Metrics {
	return prometheusMetrics{addr: addr, path: path}
}
