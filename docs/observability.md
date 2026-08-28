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

| Field           | Type   | Description                                                                                                                                                                                    |
| --------------- | ------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `ts`            | string | Request start time, RFC 3339 with nanosecond precision, UTC.                                                                                                                                   |
| `method`        | string | HTTP method.                                                                                                                                                                                   |
| `host`          | string | Host header value as received.                                                                                                                                                                 |
| `path`          | string | URL path (no query).                                                                                                                                                                           |
| `query`         | string | Raw query string (no leading `?`).                                                                                                                                                             |
| `remote`        | string | Best-effort client IP. See "client IP attribution" below.                                                                                                                                      |
| `user_agent`    | string | `User-Agent` header.                                                                                                                                                                           |
| `referer`       | string | `Referer` header.                                                                                                                                                                              |
| `status`        | int    | Response status code as committed to the client.                                                                                                                                               |
| `duration_us`   | int64  | Wall-clock duration from request start until the handler returns — for most requests that is when the response is committed; for a hijacked upgrade it is when the tunneled connection closes. |
| `proto`         | string | Protocol version, e.g. `HTTP/1.1`, `HTTP/2.0`.                                                                                                                                                 |
| `forwarded_for` | string | Raw `X-Forwarded-For` header (full chain, not parsed).                                                                                                                                         |

### Client IP attribution

The `remote` field comes from `clientIP()`, which resolves in order:

1. If the listener declares a `TrustedProxy()` policy: the policy decides — a trusted direct peer speaks through the configured forwarded header, any other peer is its own client.
2. Otherwise, on a `BehindCloudflare()` listener: `CF-Connecting-IP`, then `True-Client-IP`.
3. Fallback: `r.RemoteAddr`.

`X-Forwarded-For` is never consulted without explicit trust configuration — it is client-controlled, and rate limiting, the IP lists, and client-IP route matching all key on this value. The raw header still lands in the `forwarded_for` log field, unparsed, for forensics.

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

### Status filtering

```go
AccessLog: statute.JSONLog(statute.Stdout).Statuses("400-499", "500-599")
```

`Statuses(...)` restricts the log to requests whose final status falls in one of the given inclusive ranges — `"400-499"`, or a single status like `"404"`. Sampling controls volume but cannot express error-only logging or exclude expected successful traffic deterministically; the status filter can.

The filter is a hard gate ahead of every other logging rule, including "errors are always logged":

```text
status range filter
↓ if allowed:
    >=400 → always log
    <400  → sampling applies
```

So `Statuses("200-299")` really does suppress 500s — errors **within the selected ranges** are never sampled out, and everything outside the ranges is never logged at all. Filtering applies to the final status as committed to the client: the recorder ignores 1xx interim responses, so a 103 → 404 exchange filters as 404. Two commit edge cases are honoured — a `Flush` before any `WriteHeader` commits an implicit 200 (a later `WriteHeader(500)` cannot change what the client saw, so the filter sees 200), and 101 Switching Protocols is final, not interim, so a handler-written 101 filters as 101. A proxied upgrade takes the same path from the other side: the reverse proxy hijacks the connection and writes the 101 handshake directly to it, bypassing the response writer — but the recorder implements `Hijack` itself, so a successful hijack before any committed response latches 101. Proxied WebSocket upgrades therefore log and count as 101, and `Statuses("101")` matches them alongside handler-written 101s. The recorded duration still spans the whole tunneled connection lifetime, since the proxy's handler only returns when the tunnel closes.

`Resolve` rejects malformed or out-of-range (`[100, 599]`) inputs and normalizes the rest — sorted ascending, overlapping and adjacent ranges merged — and the canonical ranges appear in the exported schema.

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

## Health endpoint

```go
Observability: statute.Observability{
    Health: statute.Health(":8081", "/healthz"),
}
```

A dedicated process health listener for supervisors (Kubernetes probes, systemd watchdogs, load balancers). It serves exactly two paths and nothing else, with no metrics and no pprof:

- **Liveness** at the configured path (default `/healthz` when the path is empty): `200 "ok"` for the whole time the process runs.
- **Readiness** at the configured path plus `/ready` (e.g. `/healthz/ready`): `200 "ok"` once startup has committed, `503 "not ready"` otherwise.

Path matching is exact: only those two paths answer, and any other path — including the configured path with a trailing slash or anything below it — returns 404. The configured path must start with a single `/` (a `//` or `/\` opening is rejected too), must not be `/` itself, and must not end with `/`; `Resolve` rejects other shapes.

The health listener **brackets** the application's availability. `Start` binds it and begins answering first, before certificate managers start, before the initial Docker sync, and before any other socket binds — so during the entire startup phase probes read liveness `200` and readiness `503 "not ready"`. Readiness flips to `200` only when startup commits: every listener socket bound, certificate managers started, and the initial Docker sync (when configured) complete. It does **not** wait for asynchronous HTTP-01 certificate warm-up: that runs in the background after startup, and a slow or unreachable CA must not keep an otherwise-serving process out of rotation. A failed `Start` fully tears the health listener down again — socket released, serve goroutine stopped — and a retried `Start` serves health afresh.

On `Shutdown`, readiness flips to `503` as the very first action, and the health listener closes **last** — only after the content and metrics listeners have finished draining. Probes therefore keep receiving answers (liveness `200`, readiness `503`) for the whole grace period rather than refused connections; the health port refuses only once the process is done.

Like the metrics listener, the health listener is intended to be **private**: bind it to a loopback address or a private interface. It deliberately serves plain-text `ok` / `not ready` bodies with no version or subsystem detail, but a health port is still an internal surface: do not expose it publicly.

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

```text
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
