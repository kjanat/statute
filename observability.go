package statute

import (
	"io"
	"os"
)

// Observability bundles the logging, metrics, and tracing configuration.
type Observability struct {
	AccessLog AccessLog
	Metrics   Metrics
	Tracing   Tracing
}

// AccessLog is a marker for an access log destination.
type AccessLog interface {
	statuteAccessLog()
}

// Metrics is a marker for a metrics endpoint configuration.
type Metrics interface {
	statuteMetrics()
}

// Tracing is a marker for a distributed-tracing exporter configuration.
type Tracing interface {
	statuteTracing()
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

type jsonLog struct {
	dest       LogWriter
	sampleRate float64
}

func (jsonLog) statuteAccessLog() {}

// JSONLog returns a structured (JSON) access log writing to the given
// destination. By default every request is logged; use Sample to record only
// a fraction of requests at high traffic volumes.
func JSONLog(dest LogWriter) *jsonLog { return &jsonLog{dest: dest, sampleRate: 1.0} }

// Sample records a fraction of requests, in the range (0.0, 1.0]. A rate of
// 0.1 logs ten percent of requests; a rate of 1.0 (the default) logs every
// request. Errors and 5xx responses are still logged unconditionally even
// when sampling is enabled.
func (j *jsonLog) Sample(rate float64) *jsonLog {
	switch {
	case rate <= 0:
		j.sampleRate = 0
	case rate > 1:
		j.sampleRate = 1
	default:
		j.sampleRate = rate
	}
	return j
}

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

type otlpTracing struct {
	endpoint    string
	serviceName string
	insecure    bool
	sampleRate  float64
}

func (*otlpTracing) statuteTracing() {}

// OTLP configures distributed tracing via OTLP/gRPC to the given collector
// endpoint (for example "otel-collector:4317"). Spans are produced for every
// incoming request with HTTP semantic conventions, and W3C trace context is
// propagated to upstream backends.
func OTLP(endpoint string) *otlpTracing {
	return &otlpTracing{endpoint: endpoint, serviceName: "statute", sampleRate: 1.0}
}

// ServiceName sets the service.name resource attribute. Defaults to "statute".
func (t *otlpTracing) ServiceName(name string) *otlpTracing {
	t.serviceName = name
	return t
}

// Insecure disables TLS to the collector. Use only on trusted networks
// (local sidecar, in-cluster collector).
func (t *otlpTracing) Insecure() *otlpTracing {
	t.insecure = true
	return t
}

// Sample sets the trace sampling rate, in (0.0, 1.0]. Defaults to 1.0 (every
// request traced). At high traffic, sample at 0.01-0.05 to control collector
// cost without losing the ability to drill into representative requests.
func (t *otlpTracing) Sample(rate float64) *otlpTracing {
	switch {
	case rate <= 0:
		t.sampleRate = 0
	case rate > 1:
		t.sampleRate = 1
	default:
		t.sampleRate = rate
	}
	return t
}
