# Observability

statute exposes three observability channels: structured access logs, Prometheus metrics, and OpenTelemetry traces. They are independently optional but should all be enabled in production. This document covers what each emits, what to scrape and alert on, and how to size sample rates.

## Access log

```go
Observability: statute.Observability{
    AccessLog: statute.JSONLog(statute.Stdout),
}
```

One JSON line per request, written to the configured destination (`Stdout`, `Stderr`, or any `io.Writer` via the `LogWriter` type). The line is written **after** the response is committed, so the final status reflects upstream errors and timeouts.

### Fields

| Field           | Type   | Description                                                   |
| --------------- | ------ | ------------------------------------------------------------- |
| `ts`            | string | Request start time, RFC 3339 with nanosecond precision, UTC.  |
| `method`        | string | HTTP method.                                                  |
| `host`          | string | Host header value as received.                                |
| `path`          | string | URL path (no query).                                          |
| `query`         | string | Raw query string (no leading `?`).                            |
| `remote`        | string | Best-effort client IP. See "client IP attribution" below.     |
| `user_agent`    | string | `User-Agent` header.                                          |
| `referer`       | string | `Referer` header.                                             |
| `status`        | int    | Response status code as committed to the client.              |
| `duration_us`   | int64  | Wall-clock duration from request start to response committed. |
| `proto`         | string | Protocol version, e.g. `HTTP/1.1`, `HTTP/2.0`.                |
| `forwarded_for` | string | Raw `X-Forwarded-For` header (full chain, not parsed).        |

### Client IP attribution

The `remote` field comes from `clientIP()`, which checks headers in order:

1. If the request was received on a `BehindCloudflare()` listener: `CF-Connecting-IP`, then `True-Client-IP`.
2. Otherwise: first entry of `X-Forwarded-For` if present.
3. Fallback: `r.RemoteAddr`.

Headers from non-CF sources are forgeable. Only enable `BehindCloudflare()` when the listener is actually fronted by Cloudflare; otherwise clients can dictate their own `remote` value.

### Sampling

```go
AccessLog: statute.JSONLog(statute.Stdout).Sample(0.1)
```

`Sample(rate)` records a fraction of successful (status < 400) requests. Errors are always logged regardless. This means your error log volume stays informative even at low sampling rates — you see every 5xx, every 4xx, and a representative slice of successes.

Recommended rates by traffic volume:

- **<100 RPS**: `1.0` (no sampling). Log volume is manageable.
- **100–1000 RPS**: `0.1`. ~10x reduction.
- **>1000 RPS**: `0.01`. Combine with metrics for traffic shape; lean on tracing for individual-request visibility.

Sampling uses `math/rand/v2.Float64()`. The decision is independent per request, so the actual recorded fraction varies but converges to the rate over time.

## Metrics

```go
Observability: statute.Observability{
    Metrics: statute.Prometheus(":9090", "/metrics"),
}
```

Prometheus exposition format on a separate listener. The metrics listener is intended to be **private** — bind it to a loopback address or a private interface and scrape it from your monitoring system. Do not expose it publicly: the same listener exposes `pprof` (see below).

### Metric names

| Name                                             | Type    | Description                                       |
| ------------------------------------------------ | ------- | ------------------------------------------------- |
| `statute_requests_total`                         | counter | Total HTTP requests handled across all listeners. |
| `statute_requests_by_status_total{status="..."}` | counter | Requests broken down by response status code.     |
| `statute_request_duration_microseconds_sum`      | counter | Sum of request durations in microseconds.         |
| `statute_request_duration_microseconds_count`    | counter | Count of observed requests.                       |

Average request duration is `sum / count`. Histogram buckets are not exported — for percentile queries use OpenTelemetry tracing or a richer metrics backend. The current metrics surface is intentionally minimal; deployments that need more should swap the in-process `stats` for the [prometheus/client_golang](https://github.com/prometheus/client_golang) library and define their own histograms.

### What to alert on

The minimum-useful alert set:

- **Error rate**: `rate(statute_requests_by_status_total{status=~"5.."}[5m]) / rate(statute_requests_total[5m]) > 0.01`. Page when 5xx exceeds 1% over a 5-minute window.
- **Availability of upstreams**: best detected from access log (502 spike) since active health checks demote silently. Pair with backend-side metrics if you have them.
- **Latency**: `rate(statute_request_duration_microseconds_sum[5m]) / rate(statute_request_duration_microseconds_count[5m])` against a per-route SLO. Average latency is a weak signal — prefer p95/p99 from traces.

### pprof

The metrics listener also serves Go's standard pprof endpoints:

- `/debug/pprof/` — index
- `/debug/pprof/profile` — CPU profile (?seconds=30 for length)
- `/debug/pprof/heap` — heap snapshot
- `/debug/pprof/goroutine` — goroutine stacks
- `/debug/pprof/trace` — execution trace
- `/debug/pprof/cmdline`, `/debug/pprof/symbol`

Use `go tool pprof http://localhost:9090/debug/pprof/profile` for live profiling. Because pprof and metrics share a listener, the same "do not expose publicly" warning applies.

## Tracing

```go
Observability: statute.Observability{
    Tracing: statute.OTLP("otel-collector:4317").
        ServiceName("edge-proxy").
        Insecure().
        Sample(0.05),
}
```

OTLP/gRPC export to an OpenTelemetry collector. Spans use HTTP semantic conventions via [otelhttp](https://pkg.go.dev/go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp). W3C trace context is automatically extracted from incoming requests and injected into outgoing reverse-proxy requests, so traces continue across hops if your backends are also instrumented.

### Span structure

For a single proxied request you'll see:

```
statute.request                          [server span, root or child of incoming traceparent]
└── HTTP GET                             [client span, the proxy call to the backend]
    └── (backend's spans, if instrumented)
```

The server span's name is `<method> <path>` (e.g. `GET /api/v1/users`). HTTP semantic-convention attributes are populated automatically: `http.method`, `http.target`, `http.status_code`, `http.user_agent`, `http.host`, `net.peer.ip`, `net.peer.port`. The HTTP version (`http.flavor`) reflects the listener: 1.1, 2, or 3.

### Sampling

`Sample(rate)` uses `TraceIDRatioBased` with `ParentBased` outer sampling. This means:

- **Roots**: a fresh trace is sampled at `rate`. With `0.05`, 5% of root requests get traced.
- **Continuations**: a request that arrives with an existing `traceparent` honours the parent's `sampled` flag. If the upstream sampled, this hop samples too; if not, it doesn't.

This preserves trace continuity. If you trace 5% at the edge and 100% at a backend service, the backend honours the edge's decision — you never see a half-traced request where the edge span is missing. Conversely, an internal client that always samples its requests will produce fully-sampled traces through your statute proxy regardless of statute's own rate.

Recommended rates:

- **Development**: `1.0`. Trace everything.
- **Production, high-traffic**: `0.01–0.05`. Combined with parent-based sampling, errors and slow paths stay traceable when upstream services raise their sample rate for affected requests.
- **Production, low-traffic**: `0.1–1.0`. The collector cost is the limiting factor; a few thousand traces per second is comfortable for most setups.

### Resource attributes

Spans are tagged with:

- `service.name` — from `ServiceName()`. Defaults to `"statute"`.
- `statute.version` — currently `"0.1.0"`, baked into the binary.
- Process attributes (PID, runtime.name, runtime.version) via `resource.WithProcess()`.
- Telemetry SDK attributes (otel.library.name, otel.library.version) automatically.
- Anything in `OTEL_RESOURCE_ATTRIBUTES` env var (read by `resource.WithFromEnv()`).

The `OTEL_RESOURCE_ATTRIBUTES` channel is the canonical way to inject deployment-specific attributes (`environment=prod`, `region=us-east-1`, `host=edge-01`) without recompiling.

### Endpoint format

The `OTLP("...")` argument is a host:port for the gRPC OTLP collector. No scheme: gRPC clients don't take URLs. Examples:

- `OTLP("otel-collector:4317")` — typical Kubernetes sidecar/sibling
- `OTLP("localhost:4317")` — agent on the same host
- `OTLP("api.honeycomb.io:443")` — direct to a managed backend

Use `Insecure()` only when the collector is on a trusted network (sidecar, in-cluster). Managed backends always require TLS.

### Graceful shutdown

OTel's batch span processor flushes pending spans at process exit. statute calls `tp.Shutdown(ctx)` during graceful shutdown, with the same `Shutdown.GracePeriod` as the listeners. Spans queued during the last few seconds before SIGTERM are exported reliably.

If the collector is unreachable at shutdown time, the flush blocks until the grace period expires, then the process exits with the spans dropped. Alarming on collector availability matters: a chronic collector outage causes shutdowns to hang, which delays deployments.

## Combining the channels

A typical production deployment has:

- **Access log** at sample rate `0.1`, sent to a log aggregator (Loki, Cloud Logging, Logstash). Used for ad-hoc investigation and request-level forensics.
- **Metrics** scraped at 15-second intervals by Prometheus. Used for dashboards and alerts.
- **Tracing** at sample rate `0.05`, exported to an OTel collector that fans out to Honeycomb / Tempo / Jaeger. Used for latency analysis and dependency mapping.

Errors are visible in all three: access log captures every 4xx/5xx unconditionally, metrics counts them by status, and traces capture them via parent-based sampling. The redundancy is intentional — losing one channel still leaves error visibility intact.
